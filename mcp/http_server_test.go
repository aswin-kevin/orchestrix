package mcp

import (
	"io"
	"net/http"
	"strings"
	"testing"
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
	connectHTTP(t, HTTPConfig{URL: ts.URL}, ClientOptions{})
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
