package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testRetry replaces DefaultSSERetry in tests, so a reconnect loop would show
// up as many requests within a short wait.
const testRetry = 10 * time.Millisecond

// recorded is one request a recorder saw.
type recorded struct {
	method    string // HTTP method
	rpcMethod string // JSON-RPC method of a POSTed request or notification
	header    http.Header
	// response facts
	status      int
	contentType string
	sessionID   string
}

// recorder wraps a handler: it records every request and lets a test answer
// some requests itself.
type recorder struct {
	next http.Handler
	// getOpen receives a value whenever a GET stream starts (status 200).
	getOpen chan struct{}

	mu sync.Mutex
	// intercept, when set, may answer a request instead of next; it returns
	// whether it did.
	intercept func(w http.ResponseWriter, r *http.Request, rpcMethod string) bool
	reqs      []recorded
}

// setIntercept installs f as the request interceptor.
func (rec *recorder) setIntercept(f func(w http.ResponseWriter, r *http.Request, rpcMethod string) bool) {
	rec.mu.Lock()
	rec.intercept = f
	rec.mu.Unlock()
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var rpcMethod string
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		var m struct{ Method string }
		_ = json.Unmarshal(body, &m)
		rpcMethod = m.Method
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	rw := &recordingWriter{ResponseWriter: w}
	if r.Method == http.MethodGet {
		rw.onHeader = func(status int) {
			if status == http.StatusOK {
				select {
				case rec.getOpen <- struct{}{}:
				default: // nobody is counting this many streams
				}
			}
		}
	}
	entry := recorded{method: r.Method, rpcMethod: rpcMethod, header: r.Header.Clone()}
	rec.mu.Lock()
	i := len(rec.reqs)
	rec.reqs = append(rec.reqs, entry)
	rec.mu.Unlock()

	rec.mu.Lock()
	intercept := rec.intercept
	rec.mu.Unlock()
	if intercept == nil || !intercept(rw, r, rpcMethod) {
		rec.next.ServeHTTP(rw, r)
	}

	rec.mu.Lock()
	rec.reqs[i].status = rw.status
	rec.reqs[i].contentType = w.Header().Get("Content-Type")
	rec.reqs[i].sessionID = w.Header().Get("Mcp-Session-Id")
	rec.mu.Unlock()
}

func (rec *recorder) requests() []recorded {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]recorded(nil), rec.reqs...)
}

// count returns how many requests used the HTTP method.
func (rec *recorder) count(method string) int {
	n := 0
	for _, r := range rec.requests() {
		if r.method == method {
			n++
		}
	}
	return n
}

// sessionID returns the session id the server assigned at initialize.
func (rec *recorder) sessionID(t *testing.T) string {
	t.Helper()
	for _, r := range rec.requests() {
		if r.rpcMethod == "initialize" && r.sessionID != "" {
			return r.sessionID
		}
	}
	t.Fatal("no session id was assigned")
	return ""
}

// waitGETOpen waits for a GET stream to start.
func (rec *recorder) waitGETOpen(t *testing.T) {
	t.Helper()
	select {
	case <-rec.getOpen:
	case <-time.After(2 * time.Second):
		t.Fatal("no GET stream opened within 2s")
	}
}

// recordingWriter remembers the status and reports when headers are sent.
type recordingWriter struct {
	http.ResponseWriter
	status   int
	onHeader func(status int)
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		if w.onHeader != nil {
			w.onHeader(status)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer's Flush.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serveHTTP serves s over HTTP behind a recorder; the server closes when the
// test ends.
func serveHTTP(t *testing.T, s *Server, opts HandlerOptions) (*recorder, *httptest.Server) {
	t.Helper()
	rec := &recorder{next: s.Handler(opts), getOpen: make(chan struct{}, 16)}
	ts := httptest.NewServer(rec)
	t.Cleanup(ts.Close)
	return rec, ts
}

// dialTransport returns an HTTP transport whose reconnect delay is testRetry.
func dialTransport(t *testing.T, cfg HTTPConfig) *httpTransport {
	t.Helper()
	tr, err := DialHTTP(cfg)
	if err != nil {
		t.Fatalf("DialHTTP: %v", err)
	}
	ht := tr.(*httpTransport)
	ht.retry = testRetry
	return ht
}

// connectHTTP returns a client over HTTP, closed when the test ends.
func connectHTTP(t *testing.T, cfg HTTPConfig, opts ClientOptions) *Client {
	t.Helper()
	c, err := NewClient(context.Background(), dialTransport(t, cfg), opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// notifications returns ClientOptions that pass notification methods to the
// returned channel.
func notifications() (ClientOptions, <-chan string) {
	ch := make(chan string, 16)
	return ClientOptions{OnNotification: func(method string, _ json.RawMessage) {
		select {
		case ch <- method:
		default:
		}
	}}, ch
}

// waitNotification waits up to 2s for the named notification.
func waitNotification(t *testing.T, ch <-chan string, method string) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == method {
				return
			}
		case <-timeout:
			t.Fatalf("no %s within 2s", method)
		}
	}
}

func TestHTTPJSONResponses(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{})
	c := connectHTTP(t, HTTPConfig{
		URL:         ts.URL,
		BearerToken: "tok",
		Headers:     map[string]string{"X-Custom": "yes"},
	}, ClientOptions{})

	if got := callText(t, c, "echo", `{"text":"over http"}`); got != "over http" {
		t.Fatalf("echo = %q", got)
	}
	if info := c.ServerInfo(); info.Name != "test-server" {
		t.Errorf("ServerInfo = %+v", info)
	}

	sid := rec.sessionID(t)
	reqs := rec.requests()
	if len(reqs) < 3 {
		t.Fatalf("server saw %d requests, want at least initialize, initialized and the call", len(reqs))
	}
	for i, r := range reqs {
		if got := r.header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("request %d (%s %s): Authorization = %q", i, r.method, r.rpcMethod, got)
		}
		if got := r.header.Get("X-Custom"); got != "yes" {
			t.Errorf("request %d: X-Custom = %q", i, got)
		}
		if i == 0 {
			if r.rpcMethod != "initialize" || r.header.Get("Mcp-Session-Id") != "" || r.header.Get("MCP-Protocol-Version") != "" {
				t.Errorf("first request = %s %s with session %q, version %q; want a bare initialize",
					r.method, r.rpcMethod, r.header.Get("Mcp-Session-Id"), r.header.Get("MCP-Protocol-Version"))
			}
			continue
		}
		if got := r.header.Get("Mcp-Session-Id"); got != sid {
			t.Errorf("request %d (%s %s): Mcp-Session-Id = %q, want %q", i, r.method, r.rpcMethod, got, sid)
		}
		if got := r.header.Get("MCP-Protocol-Version"); got != "2025-06-18" {
			t.Errorf("request %d: MCP-Protocol-Version = %q", i, got)
		}
		if r.method == http.MethodPost {
			if got := r.header.Get("Content-Type"); got != "application/json" {
				t.Errorf("request %d: Content-Type = %q", i, got)
			}
			if got := r.header.Get("Accept"); got != "application/json, text/event-stream" {
				t.Errorf("request %d: Accept = %q", i, got)
			}
		}
		if r.rpcMethod == "tools/call" && !strings.HasPrefix(r.contentType, "application/json") {
			t.Errorf("call answered as %q, want JSON", r.contentType)
		}
	}
}

func TestHTTPHeadersOverrideBearer(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{DisableGET: true})
	connectHTTP(t, HTTPConfig{
		URL:         ts.URL,
		BearerToken: "ignored",
		Headers:     map[string]string{"authorization": "Basic abc"},
	}, ClientOptions{})
	for i, r := range rec.requests() {
		if got := r.header.Values("Authorization"); len(got) != 1 || got[0] != "Basic abc" {
			t.Errorf("request %d: Authorization = %q, want only the configured header", i, got)
		}
	}
}

func TestHTTPSSEResponses(t *testing.T) {
	s := newTestServer(nil)
	s.AddTool(ToolInfo{Name: "progress"}, func(ctx context.Context, _ json.RawMessage) (*CallToolResult, error) {
		if err := Notify(ctx, "notifications/progress", map[string]any{"progressToken": 1, "progress": 1}); err != nil {
			return nil, err
		}
		return TextResult("done"), nil
	})
	// No GET stream: the notification can only arrive on the POST stream.
	rec, ts := serveHTTP(t, s, HandlerOptions{StreamResponses: true, DisableGET: true})
	opts, notes := notifications()
	c := connectHTTP(t, HTTPConfig{URL: ts.URL}, opts)

	if got := callText(t, c, "progress", `{}`); got != "done" {
		t.Fatalf("progress = %q", got)
	}
	// The notification precedes the response on the stream, so it has been
	// handled by the time the call returns.
	select {
	case got := <-notes:
		if got != "notifications/progress" {
			t.Fatalf("notification = %q", got)
		}
	default:
		t.Fatal("progress notification was not delivered before the result")
	}
	for _, r := range rec.requests() {
		if r.rpcMethod == "tools/call" && r.contentType != "text/event-stream" {
			t.Errorf("call answered as %q, want text/event-stream", r.contentType)
		}
	}
}

func TestHTTPListChangedViaGET(t *testing.T) {
	s := newTestServer(nil)
	rec, ts := serveHTTP(t, s, HandlerOptions{})
	opts, notes := notifications()
	connectHTTP(t, HTTPConfig{URL: ts.URL}, opts)

	rec.waitGETOpen(t)
	s.AddTool(ToolInfo{Name: "late"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
		return TextResult("late"), nil
	})
	waitNotification(t, notes, "notifications/tools/list_changed")
	for _, r := range rec.requests() {
		if r.method == http.MethodGet && r.header.Get("Accept") != "text/event-stream" {
			t.Errorf("GET Accept = %q", r.header.Get("Accept"))
		}
	}
}

func TestHTTPWithoutGET(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{DisableGET: true})
	c := connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientOptions{})
	if got := callText(t, c, "echo", `{"text":"x"}`); got != "x" {
		t.Fatalf("echo = %q", got)
	}
	// The GET starts after initialized; wait for it, then give a retry loop
	// (one GET every testRetry) time to show.
	deadline := time.Now().Add(2 * time.Second)
	for rec.count(http.MethodGet) == 0 && time.Now().Before(deadline) {
		time.Sleep(testRetry)
	}
	time.Sleep(20 * testRetry)
	if got := rec.count(http.MethodGet); got != 1 {
		t.Errorf("GET requests = %d, want exactly 1 (answered 405, never retried)", got)
	}
	if got := rec.count(http.MethodPost); got != 3 {
		t.Errorf("POST requests = %d, want 3 (initialize, initialized, call)", got)
	}
	for _, r := range rec.requests() {
		if r.method == http.MethodGet && r.status != http.StatusMethodNotAllowed {
			t.Errorf("GET status = %d, want 405", r.status)
		}
	}
}

func TestHTTPGETGivesUp(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{})
	rec.setIntercept(func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if r.Method != http.MethodGet {
			return false
		}
		http.Error(w, "no stream today", http.StatusInternalServerError)
		return true
	})
	c := connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientOptions{})

	deadline := time.Now().Add(2 * time.Second)
	for rec.count(http.MethodGet) < DefaultMaxSSERetries && time.Now().Before(deadline) {
		time.Sleep(testRetry)
	}
	time.Sleep(20 * testRetry)
	if got := rec.count(http.MethodGet); got != DefaultMaxSSERetries {
		t.Errorf("GET attempts = %d, want %d", got, DefaultMaxSSERetries)
	}
	if got := callText(t, c, "echo", `{"text":"still up"}`); got != "still up" {
		t.Errorf("echo after giving up = %q", got)
	}
}

func TestHTTPGETKeepsReconnectingAfterIdleStreams(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{})
	rec.setIntercept(func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if r.Method != http.MethodGet {
			return false
		}
		// A healthy stream that idles past the retry delay, then is closed
		// by the server (as proxies do) without having carried an event.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		time.Sleep(3 * testRetry)
		return true
	})
	connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientOptions{})

	want := DefaultMaxSSERetries + 2
	deadline := time.Now().Add(3 * time.Second)
	for rec.count(http.MethodGet) < want && time.Now().Before(deadline) {
		time.Sleep(testRetry)
	}
	if got := rec.count(http.MethodGet); got < want {
		t.Errorf("GET attempts = %d, want at least %d: idle disconnects are not failures", got, want)
	}
}

func TestHTTPGETReconnects(t *testing.T) {
	s := newTestServer(nil)
	rec, ts := serveHTTP(t, s, HandlerOptions{})
	var gets atomic.Int32
	rec.setIntercept(func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if r.Method != http.MethodGet || gets.Add(1) > 1 {
			return false
		}
		// The first stream carries one notification and then ends.
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/first\"}\n\n")
		return true
	})
	opts, notes := notifications()
	connectHTTP(t, HTTPConfig{URL: ts.URL}, opts)

	waitNotification(t, notes, "notifications/first")
	rec.waitGETOpen(t) // the first stream
	rec.waitGETOpen(t) // the reconnected one
	s.AddTool(ToolInfo{Name: "late"}, func(context.Context, json.RawMessage) (*CallToolResult, error) { return nil, nil })
	waitNotification(t, notes, "notifications/tools/list_changed")
	if got := rec.count(http.MethodGet); got != 2 {
		t.Errorf("GET requests = %d, want 2", got)
	}
}

func TestHTTPSessionExpired(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{})
	var expired atomic.Bool
	rec.setIntercept(func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if !expired.Load() || r.Header.Get("Mcp-Session-Id") == "" {
			return false
		}
		http.NotFound(w, r)
		return true
	})
	c := connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientOptions{})
	expired.Store(true)

	if _, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"x"}`)); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("CallTool error = %v, want ErrSessionExpired", err)
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("client still up 2s after the session expired")
	}
	if err := c.Err(); !errors.Is(err, ErrSessionExpired) || !errors.Is(err, ErrClosed) {
		t.Errorf("Err = %v, want ErrClosed wrapping ErrSessionExpired", err)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{DisableGET: true})
	var failing atomic.Bool
	rec.setIntercept(func(w http.ResponseWriter, _ *http.Request, rpcMethod string) bool {
		if !failing.Load() || rpcMethod != "tools/call" {
			return false
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom"+strings.Repeat("z", 2000))
		return true
	})
	c := connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientOptions{})
	failing.Store(true)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want the status and body", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("error took %v, want an immediate failure", elapsed)
	}
	if got, want := strings.Count(err.Error(), "z"), maxErrorBodyBytes-len("boom"); got != want {
		t.Errorf("error quotes %d bytes of padding, want the body cut at %d bytes (%d of padding)", got, maxErrorBodyBytes, want)
	}

	failing.Store(false)
	if got := callText(t, c, "echo", `{"text":"recovered"}`); got != "recovered" {
		t.Errorf("echo after the error = %q, want the transport still up", got)
	}
}

func TestHTTPOversizedResponse(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			_, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{StreamResponses: stream, DisableGET: true})
			c := connectHTTP(t, HTTPConfig{URL: ts.URL, MaxMessageBytes: bigResultBytes / 2}, ClientOptions{})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := c.CallTool(ctx, "big", nil)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprint(bigResultBytes/2)) {
				t.Fatalf("error = %v, want one naming the %d-byte limit", err, bigResultBytes/2)
			}
			if !stream {
				// A JSON body over the limit fails only its request.
				if got := callText(t, c, "echo", `{"text":"ok"}`); got != "ok" {
					t.Errorf("echo = %q", got)
				}
				return
			}
			// An oversized event leaves the stream unreadable: the transport ends.
			select {
			case <-c.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("transport still up after an oversized event")
			}
		})
	}
}

func TestHTTPCloseSendsDelete(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{})
	c, err := NewClient(context.Background(), dialTransport(t, HTTPConfig{URL: ts.URL}), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sid := rec.sessionID(t)
	rec.waitGETOpen(t)

	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close took %v", elapsed)
	}
	var deletes []recorded
	for _, r := range rec.requests() {
		if r.method == http.MethodDelete {
			deletes = append(deletes, r)
		}
	}
	if len(deletes) != 1 || deletes[0].header.Get("Mcp-Session-Id") != sid || deletes[0].status != http.StatusOK {
		t.Fatalf("DELETE requests = %+v, want one for session %q answered 200", deletes, sid)
	}
	if got := rawPost(t, ts.URL, sid, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, nil); got != http.StatusNotFound {
		t.Errorf("POST to the deleted session = %d, want 404", got)
	}
}

func TestHTTPNoGoroutineLeak(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{StreamResponses: true})
	baseline := runtime.NumGoroutine()

	c, err := NewClient(context.Background(), dialTransport(t, HTTPConfig{URL: ts.URL}), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rec.waitGETOpen(t)
	if got := callText(t, c, "echo", `{"text":"x"}`); got != "x" {
		t.Fatalf("echo = %q", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d, baseline %d\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHTTPCallsAfterCloseFail(t *testing.T) {
	_, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{DisableGET: true})
	tr := dialTransport(t, HTTPConfig{URL: ts.URL})
	c, err := NewClient(context.Background(), tr, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if err := tr.Send(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","method":"x"}`)); !errors.Is(err, ErrClosed) {
		t.Errorf("Send after Close = %v, want ErrClosed", err)
	}
	if _, ok := <-tr.Messages(); ok {
		t.Error("Messages still open after Close")
	}
	if err := tr.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

func TestDialHTTPValidatesURL(t *testing.T) {
	for _, bad := range []string{"", "ftp://example.test/mcp", "http://", "/relative", "http://[::1"} {
		if _, err := DialHTTP(HTTPConfig{URL: bad}); err == nil {
			t.Errorf("DialHTTP(%q) succeeded, want an error", bad)
		}
	}
	// Valid URLs dial without I/O: nothing listens on port 1.
	for _, good := range []string{"http://127.0.0.1:1/mcp", "https://127.0.0.1:1/mcp"} {
		tr, err := DialHTTP(HTTPConfig{URL: good})
		if err != nil {
			t.Fatalf("DialHTTP(%q): %v", good, err)
		}
		if err := tr.Close(); err != nil {
			t.Errorf("Close without a session: %v", err)
		}
	}
}

func TestHTTPIdleSessionExpires(t *testing.T) {
	_, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{DisableGET: true, SessionIdleTimeout: 5 * testRetry})
	c := connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientOptions{})
	time.Sleep(15 * testRetry)
	if _, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"x"}`)); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("CallTool after idling = %v, want ErrSessionExpired", err)
	}
}

func TestHTTPSessionWithOpenStreamDoesNotExpire(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{SessionIdleTimeout: 5 * testRetry})
	c := connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientOptions{})
	rec.waitGETOpen(t)
	time.Sleep(15 * testRetry)
	// A new session sweeps expired ones; the busy session must survive it.
	if status, _ := rawInitialize(t, ts.URL, initializeBody); status != http.StatusOK {
		t.Fatalf("second initialize = %d", status)
	}
	if got := callText(t, c, "echo", `{"text":"alive"}`); got != "alive" {
		t.Errorf("echo = %q", got)
	}
}

func TestHTTPRefusesRedirects(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var reached atomic.Int32
			var leaked sync.Map // header or body seen by the second server
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				if v := r.Header.Get("X-Api-Key"); v != "" {
					leaked.Store("header", v)
				}
				if body, _ := io.ReadAll(r.Body); len(body) > 0 {
					leaked.Store("body", string(body))
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			defer other.Close()
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL, status)
			}))
			defer first.Close()

			for _, client := range []*http.Client{nil, {}} {
				tr, err := DialHTTP(HTTPConfig{
					URL:     first.URL,
					Headers: map[string]string{"X-Api-Key": "secret"},
					Client:  client,
				})
				if err != nil {
					t.Fatalf("DialHTTP: %v", err)
				}
				err = tr.Send(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
				tr.Close()
				if err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) || !strings.Contains(err.Error(), "redirect") {
					t.Errorf("Send (own client %v) error = %v, want a redirect error naming %d", client == nil, err, status)
				}
			}
			if n := reached.Load(); n != 0 {
				t.Errorf("the redirect target received %d requests, want none", n)
			}
			leaked.Range(func(k, v any) bool {
				t.Errorf("the redirect target received the %v %q", k, v)
				return true
			})
		})
	}
}

// fakeClock is a settable clock for a handler's session expiry.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// serveWithClock serves s's handler with its clock replaced by clock.
func serveWithClock(t *testing.T, s *Server, opts HandlerOptions, clock *fakeClock) *httptest.Server {
	t.Helper()
	h := s.Handler(opts).(*httpHandler)
	h.now = clock.Now
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestUninitializedSessionsEvicted(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	ts := serveWithClock(t, newTestServer(nil), HandlerOptions{MaxSessions: 4}, clock)
	for i := range 4 {
		if status, sid := rawInitialize(t, ts.URL, initializeBody); status != http.StatusOK || sid == "" {
			t.Fatalf("initialize %d: status %d, id %q", i, status, sid)
		}
	}
	if status, _ := rawInitialize(t, ts.URL, initializeBody); status != http.StatusServiceUnavailable {
		t.Fatalf("5th initialize while the others are fresh = %d, want 503", status)
	}
	clock.advance(pendingSessionTTL)
	if status, sid := rawInitialize(t, ts.URL, initializeBody); status != http.StatusOK || sid == "" {
		t.Errorf("initialize once the uninitialized sessions are stale: status %d, id %q; want 200", status, sid)
	}
}

func TestInitializedSessionsKeepTheirSlots(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	ts := serveWithClock(t, newTestServer(nil), HandlerOptions{MaxSessions: 4}, clock)
	for i := range 4 {
		status, sid := rawInitialize(t, ts.URL, initializeBody)
		if status != http.StatusOK || sid == "" {
			t.Fatalf("initialize %d: status %d, id %q", i, status, sid)
		}
		if got := rawPost(t, ts.URL, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); got != http.StatusAccepted {
			t.Fatalf("initialized %d = %d, want 202", i, got)
		}
	}
	clock.advance(2 * pendingSessionTTL) // still far below the idle timeout
	if status, _ := rawInitialize(t, ts.URL, initializeBody); status != http.StatusServiceUnavailable {
		t.Errorf("5th initialize with 4 active sessions = %d, want 503", status)
	}
}

// goroutinesIn counts the goroutines whose stack mentions fn.
func goroutinesIn(fn string) int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), fn)
}

func TestSSEReplyClosedOnTimeout(t *testing.T) {
	blocked := make(chan error, 1)
	rec, ts := serveHTTP(t, newTestServer(blocked), HandlerOptions{StreamResponses: true, DisableGET: true})
	c := connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, "block", nil); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	select {
	case <-blocked: // the server cancelled the call and sends no response
	case <-time.After(2 * time.Second):
		t.Fatal("the server never saw the cancellation")
	}

	// The reply stream carries nothing more for the timed-out call: the
	// client must close it rather than hold the connection open.
	deadline := time.Now().Add(2 * time.Second)
	for {
		served := false
		for _, r := range rec.requests() {
			if r.rpcMethod == "tools/call" && r.status != 0 {
				served = true
			}
		}
		relays := goroutinesIn("(*httpTransport).relay")
		if served && relays == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the timeout: server response finished = %v, client relay goroutines = %d", served, relays)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
