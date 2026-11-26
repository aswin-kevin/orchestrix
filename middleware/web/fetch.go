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
	// URL is the http or https URL to fetch.
	URL string `json:"url" desc:"An http or https URL."`
	// StartIndex is the character offset to continue a truncated page from.
	StartIndex int `json:"start_index,omitempty" desc:"Character offset to continue a truncated page from."`
	// MaxChars is the maximum number of characters to return.
	MaxChars int `json:"max_chars,omitempty" desc:"Maximum characters to return."`
}

// fetcher runs web_fetch over a Fetcher.
type fetcher struct {
	src      Fetcher
	maxBytes int64
	maxChars int
	timeout  time.Duration
}

// newFetchTool returns the web_fetch runner of cfg reading through src.
func newFetchTool(src Fetcher, cfg Config) *fetcher {
	return &fetcher{
		src:      src,
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
	page, err := f.src.Fetch(ctx, u, f.maxBytes)
	if err == nil && page == nil {
		err = errors.New("web: Fetcher returned no page")
	}
	if err != nil {
		return tool.Errorf("fetching %s failed: %s", u, describe(err)), nil
	}
	finalURL := page.URL
	if finalURL == "" {
		finalURL = u.String()
	}
	if page.Status >= http.StatusBadRequest {
		// The status is the error the model needs. The text, cut to
		// errorBodyChars characters, is only context: an error page in an
		// unsupported type quotes nothing.
		msg := fmt.Sprintf("HTTP %d for %s", page.Status, finalURL)
		if quote := []rune(strings.TrimSpace(page.Text)); len(quote) > 0 {
			msg += "\n\n" + string(quote[:min(len(quote), errorBodyChars)])
		}
		return tool.Errorf("%s", msg), nil
	}
	text := page.Text
	if page.Truncated {
		text += fmt.Sprintf("\n\n[response truncated at %d bytes]", f.maxBytes)
	}
	return f.page(in, finalURL, page.Title, text), nil
}

// HTTPFetcher is the default Fetcher: it reads pages over HTTP with a client
// from NewHTTPClient, so it enforces that client's network policy, the
// private-address guard included. HTML is converted to markdown; text, JSON
// and XML are returned as they are; other content types are an error. It
// is safe for concurrent use.
type HTTPFetcher struct {
	client *http.Client
}

// NewHTTPFetcher returns an HTTPFetcher whose client NewHTTPClient builds
// from cc. It fails when NewHTTPClient does.
func NewHTTPFetcher(cc ClientConfig) (*HTTPFetcher, error) {
	client, err := NewHTTPClient(cc)
	if err != nil {
		return nil, err
	}
	return &HTTPFetcher{client: client}, nil
}

// Fetch implements Fetcher. It reads at most maxBytes of the body (≤ 0
// means DefaultMaxBytes; Page.Truncated reports the cut) and converts HTML
// to at most maxBytes of markdown, within ctx; markdown cut there ends with
// a "[content truncated]" note. The page of a status of 400 or more carries
// whatever text its body converted to; a conversion that fails or that ctx
// cuts short is otherwise an error.
func (f *HTTPFetcher) Fetch(ctx context.Context, u *url.URL, maxBytes int64) (*Page, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	resp, err := f.get(ctx, u, maxBytes)
	if err != nil {
		return nil, err
	}
	title, text, truncated, convErr := convert(ctx, resp, maxBytes)
	page := &Page{URL: resp.url.String(), Status: resp.status, Title: title, Text: text, Truncated: resp.truncated}
	if resp.status >= http.StatusBadRequest {
		return page, nil
	}
	if convErr != nil {
		// Only a conversion that fails is an error: content that was ready
		// in time is returned even if ctx's deadline has passed since.
		return nil, convErr
	}
	if truncated {
		page.Text += "\n\n[content truncated]"
	}
	return page, nil
}

// response is what a request read.
type response struct {
	url         *url.URL // after redirects
	status      int
	contentType string
	body        []byte
	truncated   bool // body was cut at maxBytes
}

// get performs the request and reads at most maxBytes of the body. Closing
// the body unread stops the transfer.
func (f *HTTPFetcher) get(ctx context.Context, u *url.URL, maxBytes int64) (*response, error) {
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	r := &response{url: resp.Request.URL, status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body}
	if int64(len(body)) > maxBytes {
		r.body, r.truncated = body[:maxBytes], true
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
func convert(ctx context.Context, r *response, maxBytes int64) (title, text string, truncated bool, err error) {
	ct := r.contentType
	if ct == "" {
		ct = http.DetectContentType(r.body)
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", "", false, fmt.Errorf("web: unsupported content type %q", ct)
	}
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		return HTMLToMarkdown(ctx, bytes.NewReader(r.body), r.url, int(min(maxBytes, math.MaxInt32)))
	case textual(mediaType):
		return "", strings.ToValidUTF8(string(r.body), "�"), false, nil
	}
	return "", "", false, fmt.Errorf("web: unsupported content type %s", mediaType)
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
