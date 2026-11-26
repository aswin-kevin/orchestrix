package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rawPost POSTs body to url with the given session id and extra headers and
// returns the status.
func rawPost(t *testing.T, url, sessionID, body string, header map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`

func TestHTTPHandlerRejects(t *testing.T) {
	_, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{DisableGET: true})
	// A real session, for the cases that need one.
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(initializeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != http.StatusOK || len(sid) < 32 {
		t.Fatalf("initialize: status %d, session id %q; want 200 and a random id", resp.StatusCode, sid)
	}

	ping := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	tests := []struct {
		name    string
		session string
		body    string
		header  map[string]string
		want    int
	}{
		{"known session", sid, ping, nil, http.StatusOK},
		{"notification accepted", sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil, http.StatusAccepted},
		{"response accepted", sid, `{"jsonrpc":"2.0","id":9,"result":{}}`, nil, http.StatusAccepted},
		{"missing session", "", ping, nil, http.StatusBadRequest},
		{"unknown session", "nope", ping, nil, http.StatusNotFound},
		{"invalid JSON", sid, `{"jsonrpc":`, nil, http.StatusBadRequest},
		{"batch", sid, "[" + ping + "]", nil, http.StatusBadRequest},
		{"unsupported version", sid, ping, map[string]string{"MCP-Protocol-Version": "1999-01-01"}, http.StatusBadRequest},
		{"foreign origin", sid, ping, map[string]string{"Origin": "http://evil.test"}, http.StatusForbidden},
		{"same origin", sid, ping, map[string]string{"Origin": ts.URL}, http.StatusOK},
		{"oversized body", sid, `{"jsonrpc":"2.0","id":3,"method":"ping","params":{"pad":"` + strings.Repeat("p", DefaultMaxMessageBytes) + `"}}`, nil, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rawPost(t, ts.URL, tt.session, tt.body, tt.header); got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}

	for _, tt := range []struct {
		method, session string
		want            int
	}{
		{http.MethodGet, sid, http.StatusMethodNotAllowed},
		{http.MethodPut, sid, http.StatusMethodNotAllowed},
		{http.MethodDelete, "", http.StatusBadRequest},
		{http.MethodDelete, "nope", http.StatusNotFound},
		{http.MethodDelete, sid, http.StatusOK},
		{http.MethodDelete, sid, http.StatusNotFound},
	} {
		req, _ := http.NewRequest(tt.method, ts.URL, nil)
		if tt.session != "" {
			req.Header.Set("Mcp-Session-Id", tt.session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s (session %q) = %d, want %d", tt.method, tt.session, resp.StatusCode, tt.want)
		}
	}
}

func TestHTTPHandlerSecondGETConflicts(t *testing.T) {
	rec, ts := serveHTTP(t, newTestServer(nil), HandlerOptions{})
	connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientConfig{})
	rec.waitGETOpen(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", rec.sessionID(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second GET = %d, want 409", resp.StatusCode)
	}
}

// rawInitialize POSTs an initialize request with body and returns the status
// and the session id the server named.
func rawInitialize(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Mcp-Session-Id")
}

// serveHandler serves s's handler directly, returning it for inspection.
func serveHandler(t *testing.T, s *Server, opts HandlerOptions) (*httpHandler, *httptest.Server) {
	t.Helper()
	h := s.Handler(opts).(*httpHandler)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return h, ts
}

func (h *httpHandler) sessionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

func TestHTTPFailedInitializeLeavesNoSession(t *testing.T) {
	h, ts := serveHandler(t, newTestServer(nil), HandlerOptions{})
	status, sid := rawInitialize(t, ts.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":"not an object"}`)
	if status != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200 carrying a JSON-RPC error", status)
	}
	if n := h.sessionCount(); n != 0 {
		t.Errorf("sessions after a failed initialize = %d, want 0", n)
	}
	if sid != "" {
		if got := rawPost(t, ts.URL, sid, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, nil); got != http.StatusNotFound {
			t.Errorf("POST with the failed session's id = %d, want 404", got)
		}
	}
}

func TestHTTPMaxSessions(t *testing.T) {
	h, ts := serveHandler(t, newTestServer(nil), HandlerOptions{MaxSessions: 2})
	var ids []string
	for i := range 2 {
		status, sid := rawInitialize(t, ts.URL, initializeBody)
		if status != http.StatusOK || sid == "" {
			t.Fatalf("initialize %d: status %d, id %q", i, status, sid)
		}
		ids = append(ids, sid)
	}
	if status, sid := rawInitialize(t, ts.URL, initializeBody); status != http.StatusServiceUnavailable || sid != "" {
		t.Fatalf("initialize over the cap: status %d, id %q; want 503 and no session", status, sid)
	}
	if n := h.sessionCount(); n != 2 {
		t.Errorf("sessions = %d, want 2", n)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", ids[0])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if status, _ := rawInitialize(t, ts.URL, initializeBody); status != http.StatusOK {
		t.Errorf("initialize after a DELETE freed a slot = %d, want 200", status)
	}
}

func TestHTTPIdleSessionsSwept(t *testing.T) {
	const idle = 50 * time.Millisecond
	h, ts := serveHandler(t, newTestServer(nil), HandlerOptions{MaxSessions: 1, SessionIdleTimeout: idle})
	_, first := rawInitialize(t, ts.URL, initializeBody)
	time.Sleep(3 * idle)
	if got := rawPost(t, ts.URL, first, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, nil); got != http.StatusNotFound {
		t.Errorf("POST to an idle session = %d, want 404", got)
	}
	if n := h.sessionCount(); n != 0 {
		t.Errorf("sessions after the expired one was touched = %d, want 0", n)
	}

	// An expired session that is never touched again does not hold a slot.
	_, second := rawInitialize(t, ts.URL, initializeBody)
	if second == "" {
		t.Fatal("no session for the second client")
	}
	time.Sleep(3 * idle)
	if status, _ := rawInitialize(t, ts.URL, initializeBody); status != http.StatusOK {
		t.Errorf("initialize with only an expired session registered = %d, want 200", status)
	}
}

// startBlockingCall initializes a session on a server whose "wait" tool
// blocks until its ctx ends, POSTs a call with request id 7 in the
// background and returns the session id and the call's status channel once
// the handler is running.
func startBlockingCall(t *testing.T, opts HandlerOptions) (url, sid string, status <-chan int) {
	t.Helper()
	s := newTestServer(nil)
	started := make(chan struct{}, 1)
	s.AddTool(ToolInfo{Name: "wait"}, func(ctx context.Context, _ json.RawMessage) (*CallToolResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	_, ts := serveHandler(t, s, opts)
	_, sid = rawInitialize(t, ts.URL, initializeBody)
	// Runs before the server's Close (cleanups run last-in first-out), which
	// would otherwise wait for the blocked call forever.
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
		req.Header.Set("Mcp-Session-Id", sid)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	})
	ch := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, ts.URL,
			strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wait"}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Mcp-Session-Id", sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- 0
			return
		}
		_ = resp.Body.Close()
		ch <- resp.StatusCode
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the wait tool never started")
	}
	return ts.URL, sid, ch
}

func TestHTTPPostEndedByDeleteIs404(t *testing.T) {
	url, sid, status := startBlockingCall(t, HandlerOptions{DisableGET: true})
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case got := <-status:
		if got != http.StatusNotFound {
			t.Errorf("POST cut off by DELETE = %d, want 404", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("POST still open 2s after its session was deleted")
	}
}

func TestHTTPDuplicateInFlightRequestID(t *testing.T) {
	url, sid, _ := startBlockingCall(t, HandlerOptions{DisableGET: true})
	if got := rawPost(t, url, sid, `{"jsonrpc":"2.0","id":7,"method":"ping"}`, nil); got != http.StatusBadRequest {
		t.Errorf("request reusing an in-flight id = %d, want 400", got)
	}
	if got := rawPost(t, url, sid, `{"jsonrpc":"2.0","id":8,"method":"ping"}`, nil); got != http.StatusOK {
		t.Errorf("request with a fresh id = %d, want 200", got)
	}
}

func TestHTTPOriginHostIsCaseInsensitive(t *testing.T) {
	_, ts := serveHandler(t, newTestServer(nil), HandlerOptions{DisableGET: true, AllowedHosts: []string{"mcp.example.test"}})
	for _, tt := range []struct {
		origin string
		want   int
	}{
		{"http://MCP.Example.Test", http.StatusOK},
		{"http://other.example.test", http.StatusForbidden},
	} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(initializeBody))
		req.Host = "mcp.example.test"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", tt.origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("Origin %s = %d, want %d", tt.origin, resp.StatusCode, tt.want)
		}
	}
}

// hostStatus POSTs initialize to url with the given Host and Origin headers
// (empty → none) and returns the status.
func hostStatus(t *testing.T, url, host, origin string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(initializeBody))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestHTTPDefaultAllowedHostsAreLoopback(t *testing.T) {
	_, ts := serveHandler(t, newTestServer(nil), HandlerOptions{DisableGET: true})
	for _, tt := range []struct {
		host, origin string
		want         int
	}{
		// A DNS-rebinding page: its origin and the Host it sends agree.
		{"evil.example:8080", "http://evil.example:8080", http.StatusForbidden},
		{"evil.example", "", http.StatusForbidden},
		{"localhost.evil.example", "", http.StatusForbidden},
		{"127.0.0.1:9", "", http.StatusOK},
		{"LocalHost:1234", "http://localhost:1234", http.StatusOK},
		{"localhost", "", http.StatusOK},
		{"[::1]:8080", "", http.StatusOK},
		{"127.0.0.1:9", "http://evil.example", http.StatusForbidden}, // the Origin check still applies
	} {
		if got := hostStatus(t, ts.URL, tt.host, tt.origin); got != tt.want {
			t.Errorf("Host %q, Origin %q = %d, want %d", tt.host, tt.origin, got, tt.want)
		}
	}
}

func TestHTTPAllowedHosts(t *testing.T) {
	_, ts := serveHandler(t, newTestServer(nil), HandlerOptions{
		DisableGET:   true,
		AllowedHosts: []string{"mcp.example.test", "pinned.example.test:8443"},
	})
	for _, tt := range []struct {
		host string
		want int
	}{
		{"mcp.example.test", http.StatusOK},
		{"MCP.example.test:9000", http.StatusOK},
		{"pinned.example.test:8443", http.StatusOK},
		{"pinned.example.test:8444", http.StatusForbidden},
		{"localhost:8080", http.StatusForbidden}, // a list replaces the loopback default
		{"evil.example", http.StatusForbidden},
	} {
		if got := hostStatus(t, ts.URL, tt.host, ""); got != tt.want {
			t.Errorf("Host %q = %d, want %d", tt.host, got, tt.want)
		}
	}

	_, any := serveHandler(t, newTestServer(nil), HandlerOptions{DisableGET: true, AllowedHosts: []string{"*"}})
	if got := hostStatus(t, any.URL, "anything.example", ""); got != http.StatusOK {
		t.Errorf(`with AllowedHosts ["*"], Host anything.example = %d, want 200`, got)
	}
}
