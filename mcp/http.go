package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/internal/sse"
)

// Defaults of the streamable HTTP transport.
const (
	// DefaultSSERetry is how long the client waits before reopening its GET
	// stream after it ended or failed.
	DefaultSSERetry = time.Second
	// DefaultMaxSSERetries is how many GET attempts in a row may fail (fail
	// to connect, or end within DefaultSSERetry without delivering an
	// event) before the client stops reopening the stream.
	DefaultMaxSSERetries = 5
)

// maxErrorBodyBytes is how much of an error response's body an error quotes.
const maxErrorBodyBytes = 512

// maxDrainBytes is how much of an unused response body is read before
// closing it, so small bodies leave the connection reusable.
const maxDrainBytes = 64 << 10

// ErrSessionExpired reports that the server no longer knows the client's
// session (it answered 404 to a request carrying the session id).
var ErrSessionExpired = errors.New("mcp: session expired")

// HTTPConfig configures DialHTTP.
type HTTPConfig struct {
	// URL is the server's MCP endpoint, an absolute http or https URL.
	URL string
	// Headers are sent on every request; nil sends none.
	Headers map[string]string
	// BearerToken, when non-empty, is sent as "Authorization: Bearer
	// <token>" unless Headers sets Authorization; empty sends none.
	BearerToken string
	// Client makes the requests; nil means a client of the transport's own with
	// no overall timeout (streams are long-lived), whose idle connections
	// are closed on Close. Either way redirects are never followed (a
	// given client is copied with its CheckRedirect replaced), so Headers,
	// the bearer token and the request body never reach another URL; a 3xx
	// reply is an error naming the status.
	Client *http.Client
	// MaxMessageBytes caps one incoming message (a JSON body or an SSE
	// event); 0 means DefaultMaxMessageBytes; negative is an error.
	MaxMessageBytes int
}

// httpTransport is the client side of the streamable HTTP transport.
type httpTransport struct {
	url    string
	header http.Header // static headers of every request
	client *http.Client
	own    *http.Transport // the client's transport when it is ours; nil otherwise
	max    int
	retry  time.Duration // delay before reopening the GET stream

	ctx    context.Context // ends every request and stream when the transport ends
	cancel context.CancelFunc

	msgs     chan json.RawMessage
	done     chan struct{} // closed when the transport ends
	finished chan struct{} // closed once msgs is closed

	mu        sync.Mutex
	sessionID string
	version   string
	listening bool // the GET listener was started
	ended     bool
	err       error
	wg        sync.WaitGroup // Sends, response streams and the GET listener

	rel release
}

// DialHTTP returns a Transport speaking the streamable HTTP transport to
// cfg.URL. It only validates the configuration; no request is made until
// the first Send.
//
// Send POSTs each message. A JSON reply is delivered whole (one over
// MaxMessageBytes fails that Send and the transport stays up); an SSE reply
// is relayed event by event after Send returns, until the stream ends or
// Send's ctx does, so the caller keeps ctx alive while it waits for the
// response and ends it once it stops waiting. An event over MaxMessageBytes
// ends the transport, since the request it belongs to could not otherwise
// be failed; a reply stream that breaks before its response leaves that
// request to its timeout. The session id and the negotiated protocol
// version are sent once known. After notifications/initialized the
// transport keeps a GET stream open for server-initiated messages,
// reopening it after DefaultSSERetry until DefaultMaxSSERetries attempts in
// a row fail or the server answers 405. A 404 to a request carrying the
// session id ends the transport with ErrSessionExpired; any other error
// status fails only that Send. Close ends every stream, sends DELETE for
// the session (bounded by DefaultCloseGrace) and waits for the transport's
// goroutines.
func DialHTTP(cfg HTTPConfig) (Transport, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("mcp: http: invalid URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("mcp: http: URL %q is not an absolute http or https URL", cfg.URL)
	}
	if cfg.MaxMessageBytes < 0 {
		return nil, errors.New("mcp: MaxMessageBytes is negative")
	}
	header := http.Header{}
	for k, v := range cfg.Headers {
		header.Set(k, v)
	}
	if cfg.BearerToken != "" && header.Get("Authorization") == "" {
		header.Set("Authorization", "Bearer "+cfg.BearerToken)
	}
	t := &httpTransport{
		url:      u.String(),
		header:   header,
		max:      positiveOr(cfg.MaxMessageBytes, DefaultMaxMessageBytes),
		retry:    DefaultSSERetry,
		msgs:     make(chan json.RawMessage),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
	if cfg.Client != nil {
		c := *cfg.Client
		t.client = &c
	} else {
		t.client = &http.Client{}
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			t.own = base.Clone()
			t.client.Transport = t.own
		}
	}
	t.client.CheckRedirect = refuseRedirect
	t.ctx, t.cancel = context.WithCancel(context.Background())
	return t, nil
}

// SetProtocolVersion makes every later request carry the negotiated
// protocol version.
func (t *httpTransport) SetProtocolVersion(version string) {
	t.mu.Lock()
	t.version = version
	t.mu.Unlock()
}

// Send POSTs msg and handles the reply (see DialHTTP).
func (t *httpTransport) Send(ctx context.Context, msg json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !t.track() {
		return t.closedError()
	}
	defer t.wg.Done()

	// The request, and a reply stream after Send returns, end when ctx
	// does or the transport ends.
	reqCtx, cancel := context.WithCancel(t.ctx)
	stop := context.AfterFunc(ctx, cancel)
	streaming := false
	defer func() {
		if !streaming {
			stop()
			cancel()
		}
	}()

	req, sid, err := t.newRequest(reqCtx, http.MethodPost, msg)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mediaTypeJSON)
	req.Header.Set("Accept", mediaTypeJSON+", "+mediaTypeEventStream)
	resp, err := t.client.Do(req)
	if err != nil {
		return t.requestError(ctx, err)
	}
	if id := resp.Header.Get(headerSessionID); id != "" {
		t.mu.Lock()
		t.sessionID = id
		t.mu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusAccepted:
		drainClose(resp.Body)
	case resp.StatusCode == http.StatusNotFound && sid != "":
		drainClose(resp.Body)
		t.end(ErrSessionExpired)
		return fmt.Errorf("mcp: http: %w", ErrSessionExpired)
	case resp.StatusCode != http.StatusOK:
		return statusError(resp)
	case hasMediaType(resp, mediaTypeEventStream):
		if err := ctx.Err(); err != nil { // ctx ended as the headers arrived
			resp.Body.Close()
			return err
		}
		if !t.track() {
			resp.Body.Close()
			return t.closedError()
		}
		streaming = true
		go func() {
			defer t.wg.Done()
			defer stop()
			defer cancel()
			defer resp.Body.Close()
			if _, err := t.relay(resp.Body); errors.Is(err, sse.ErrTooLarge) {
				t.end(fmt.Errorf("mcp: http: %w", err))
			}
		}()
	case hasMediaType(resp, mediaTypeJSON):
		body, err := readLimited(resp.Body, t.max)
		drainClose(resp.Body)
		if err != nil {
			return t.requestError(ctx, err)
		}
		if len(bytes.TrimSpace(body)) > 0 && !t.push(body) {
			return t.closedError()
		}
	default:
		drainClose(resp.Body)
		return fmt.Errorf("mcp: http: POST: unexpected content type %q", resp.Header.Get("Content-Type"))
	}

	if isInitializedNotification(msg) {
		t.startListener()
	}
	return nil
}

// Messages yields the server's messages from every reply and stream.
func (t *httpTransport) Messages() <-chan json.RawMessage { return t.msgs }

// Err reports why the transport ended: ErrSessionExpired, an oversized
// event, or nil after Close.
func (t *httpTransport) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Close ends every stream, sends DELETE for the session and waits for the
// transport's goroutines. The DELETE runs in the release, bounded by
// DefaultCloseGrace and not by ctx; see Transport for the contract.
func (t *httpTransport) Close(ctx context.Context) error {
	t.rel.start(func() error {
		t.mu.Lock()
		sid, expired := t.sessionID, errors.Is(t.err, ErrSessionExpired)
		t.mu.Unlock()
		t.end(nil)
		if sid != "" && !expired {
			t.deleteSession(context.WithoutCancel(ctx))
		}
		<-t.finished
		if t.own != nil {
			t.own.CloseIdleConnections()
		}
		return nil
	})
	return t.rel.wait(ctx)
}

// track counts a goroutine or call in wg unless the transport has ended.
func (t *httpTransport) track() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return false
	}
	t.wg.Add(1)
	return true
}

// end ends the transport for reason err (nil when closed): it cancels every
// request and stream, then closes Messages once their goroutines are done.
func (t *httpTransport) end(err error) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	t.ended, t.err = true, err
	t.mu.Unlock()
	t.cancel()
	close(t.done)
	go func() {
		t.wg.Wait()
		close(t.msgs)
		close(t.finished)
	}()
}

// closedError is what calls on an ended transport return.
func (t *httpTransport) closedError() error {
	if err := t.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return ErrClosed
}

// requestError reports a failed exchange: ctx's error when the caller gave
// up, the closed error when the transport ended, otherwise err.
func (t *httpTransport) requestError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case t.ctx.Err() != nil:
		return t.closedError()
	}
	return fmt.Errorf("mcp: http: POST: %w", err)
}

// newRequest builds a request carrying the static headers, the session id
// and the protocol version as currently known; it returns the session id it
// sent.
func (t *httpTransport) newRequest(ctx context.Context, method string, body []byte) (*http.Request, string, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.url, r)
	if err != nil {
		return nil, "", fmt.Errorf("mcp: http: %w", err)
	}
	req.Header = t.header.Clone()
	t.mu.Lock()
	sid, version := t.sessionID, t.version
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set(headerSessionID, sid)
	}
	if version != "" {
		req.Header.Set(headerProtocolVersion, version)
	}
	return req, sid, nil
}

// push delivers a message unless the transport has ended.
func (t *httpTransport) push(msg json.RawMessage) bool {
	select {
	case t.msgs <- msg:
		return true
	case <-t.done:
		return false
	}
}

// deleteSession tells the server, best-effort and within
// DefaultCloseGrace, that the session is over. ctx supplies values only.
func (t *httpTransport) deleteSession(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, DefaultCloseGrace)
	defer cancel()
	req, _, err := t.newRequest(ctx, http.MethodDelete, nil)
	if err != nil {
		return
	}
	if resp, err := t.client.Do(req); err == nil {
		drainClose(resp.Body)
	}
}

// isInitializedNotification reports whether msg is
// notifications/initialized.
func isInitializedNotification(msg json.RawMessage) bool {
	var m message
	return json.Unmarshal(msg, &m) == nil && m.isNotification() && m.Method == notifyInitialized
}

// hasMediaType reports whether resp's Content-Type is mediaType.
func hasMediaType(resp *http.Response, mediaType string) bool {
	got, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && got == mediaType
}

// readLimited reads all of r, failing once it holds more than max bytes.
func readLimited(r io.Reader, max int) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > max {
		return nil, fmt.Errorf("response over %d bytes", max)
	}
	return body, nil
}

// refuseRedirect is the CheckRedirect of every transport client: the 3xx
// response is returned as is, so a redirect is never followed.
func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// statusError describes an error response by its status and the start of
// its body, and closes the body. A 3xx is reported as a refused redirect.
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	drainClose(resp.Body)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("mcp: http: POST: %s: redirect to %q not followed", resp.Status, resp.Header.Get("Location"))
	}
	return fmt.Errorf("mcp: http: POST: %s: %q", resp.Status, bytes.TrimSpace(body))
}

// drainClose reads what is left of a small body and closes it.
func drainClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
	_ = body.Close()
}
