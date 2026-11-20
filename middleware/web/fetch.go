package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/tool"
)

const (
	// acceptHeader is the Accept header of web_fetch requests.
	acceptHeader = "text/html, text/plain, application/json, */*;q=0.5"
	// errorBodyChars is how much text an HTTP error result quotes.
	errorBodyChars = 2000
)

const fetchDescription = "Fetch a web page and return its content: HTML is converted to markdown, plain text, JSON and XML are returned as they are. " +
	"Long content is returned in parts; the result then ends with a note giving the start_index to continue from. " +
	"Permission rules apply to the URL given; redirects it leads to are followed."

// FetchInput is the input of web_fetch.
type FetchInput struct {
	URL        string `json:"url" desc:"An http or https URL."`
	StartIndex int    `json:"start_index,omitempty" desc:"Character offset to continue a truncated page from."`
	MaxChars   int    `json:"max_chars,omitempty" desc:"Maximum characters to return."`
}

// fetcher runs web_fetch with a guarded client.
type fetcher struct {
	client   *http.Client
	maxBytes int64
	maxChars int
	timeout  time.Duration
}

func newFetcher(client *http.Client, cfg Config) *fetcher {
	return &fetcher{
		client:   client,
		maxBytes: positive(cfg.MaxBytes, DefaultMaxBytes),
		maxChars: positive(cfg.MaxChars, DefaultMaxChars),
		timeout:  positive(cfg.Timeout, DefaultTimeout),
	}
}

// tool returns web_fetch. Its permission subject is the canonical URL, so
// rules such as web_fetch(https://go.dev/*) apply; it has no verdict of its
// own, so a call no rule allows needs an approval unless readOnly is set.
//
// Rules match only the URL the model asked for. Redirects are followed
// without consulting the rules again; the network guard still checks every
// hop.
func (f *fetcher) tool(readOnly bool) tool.Tool {
	opts := []tool.Option{tool.WithConcurrent(), tool.WithSubject(fetchSubject)}
	if readOnly {
		opts = append(opts, tool.WithReadOnly())
	}
	return tool.New(ToolFetch, fetchDescription, f.fetch, opts...)
}

// fetchSubject returns the canonical form of the input's URL (see
// canonicalURL), which is also the URL requested. An invalid URL has the
// empty subject, which only a bare "*" pattern matches, so a URL prefix
// rule is never satisfied by text the request would not use; the call then
// fails without a request.
func fetchSubject(raw json.RawMessage) string {
	var in FetchInput
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	u, err := canonicalURL(in.URL)
	if err != nil {
		return ""
	}
	return u.String()
}

// response is what a fetch read.
type response struct {
	url         *url.URL // after redirects
	status      int
	contentType string
	body        []byte
	truncated   bool // body was cut at maxBytes
}

func (f *fetcher) fetch(ctx context.Context, in FetchInput, _ *tool.Call) (tool.Result, error) {
	u, err := canonicalURL(in.URL)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	if in.StartIndex < 0 || in.MaxChars < 0 {
		return tool.Errorf("start_index and max_chars must not be negative"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	resp, err := f.get(ctx, u)
	if err != nil {
		return tool.Errorf("fetching %s failed: %s", u, describe(err)), nil
	}

	title, text, truncated, convErr := f.convert(ctx, resp)
	if resp.status >= http.StatusBadRequest {
		// The status is the error the model needs. The converted body, cut
		// to errorBodyChars characters, is only context: an error page in
		// an unsupported type, which convert refused, quotes nothing.
		msg := fmt.Sprintf("HTTP %d for %s", resp.status, resp.url)
		if quote := []rune(strings.TrimSpace(text)); len(quote) > 0 {
			msg += "\n\n" + string(quote[:min(len(quote), errorBodyChars)])
		}
		return tool.Errorf("%s", msg), nil
	}
	if errors.Is(convErr, context.DeadlineExceeded) || errors.Is(convErr, context.Canceled) {
		// Only a conversion the deadline cut short is a timeout: content
		// that was ready in time is returned even if the deadline has
		// passed since.
		return tool.Errorf("fetching %s failed: %s", u, describe(convErr)), nil
	}
	if convErr != nil {
		return tool.Errorf("%v", convErr), nil
	}
	if truncated {
		text += "\n\n[content truncated]"
	}
	if resp.truncated {
		text += fmt.Sprintf("\n\n[response truncated at %d bytes]", f.maxBytes)
	}
	return f.page(in, resp.url.String(), title, text), nil
}

// get performs the request and reads at most maxBytes of the body. Closing
// the body unread stops the transfer.
func (f *fetcher) get(ctx context.Context, u *url.URL) (*response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", acceptHeader)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return nil, err
	}
	r := &response{url: resp.Request.URL, status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body}
	if int64(len(body)) > f.maxBytes {
		r.body, r.truncated = body[:f.maxBytes], true
	}
	return r, nil
}

// describe names the cause of a failed request for the model.
func describe(err error) string {
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		cause = ue.Err // drop the method and URL the caller already names
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return "timed out: " + cause.Error()
	}
	return cause.Error()
}

// convert turns the body into text according to its content type, sniffed
// when the server sent none. HTML is converted to at most maxBytes of
// markdown, within ctx; truncated reports that the markdown was cut.
func (f *fetcher) convert(ctx context.Context, r *response) (title, text string, truncated bool, err error) {
	ct := r.contentType
	if ct == "" {
		ct = http.DetectContentType(r.body)
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", "", false, fmt.Errorf("unsupported content type %q", ct)
	}
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		return HTMLToMarkdownContext(ctx, bytes.NewReader(r.body), r.url, int(min(f.maxBytes, math.MaxInt32)))
	case textual(mediaType):
		return "", strings.ToValidUTF8(string(r.body), "�"), false, nil
	}
	return "", "", false, fmt.Errorf("unsupported content type %s", mediaType)
}

// textual reports whether mediaType is returned as it is: text, JSON or XML.
func textual(mediaType string) bool {
	return strings.HasPrefix(mediaType, "text/") ||
		mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") ||
		mediaType == "application/xml" || strings.HasSuffix(mediaType, "+xml")
}

// page renders the part of text the input selects, by character, with a
// note naming where to continue when more remains.
func (f *fetcher) page(in FetchInput, finalURL, title, text string) tool.Result {
	runes := []rune(text)
	total := len(runes)
	start := in.StartIndex
	if start > 0 && start >= total {
		return tool.Errorf("start_index %d is past the end of the content (%d characters)", start, total)
	}
	limit := f.maxChars
	if in.MaxChars > 0 {
		limit = min(limit, in.MaxChars)
	}
	end := min(start+limit, total)

	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\nTitle: %s\n\n", finalURL, title)
	b.WriteString(string(runes[start:end]))
	if end < total {
		fmt.Fprintf(&b, "\n\n[Content truncated: characters %d–%d of %d. Call web_fetch with start_index=%d to continue.]", start, end, total, end)
	}
	return tool.Text(b.String())
}
