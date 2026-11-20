package llm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testSecret = "SECRET-0123456789"

func TestBaseURLQueryKeyNotLeaked(t *testing.T) {
	var requestURI atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI.Store(r.URL.RequestURI())
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestOpenAI(t, srv.URL+"/v1?key="+testSecret)
	_, err := Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}})
	if err == nil {
		t.Fatal("want an error from a 500 reply")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("500 error leaks the key: %v", err)
	}
	if got, want := requestURI.Load(), "/v1/chat/completions?key="+testSecret; got != want {
		t.Errorf("request URI = %v, want %q", got, want)
	}

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := closed.URL + "/v1?key=" + testSecret
	closed.Close() // connections are now refused

	p = newTestOpenAI(t, base)
	_, err = Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}})
	if err == nil {
		t.Fatal("want a network error")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("network error leaks the key: %v", err)
	}
	if !strings.Contains(err.Error(), "key=REDACTED") {
		t.Errorf("network error lost the redacted URL: %v", err)
	}
	if !IsRetryable(err) {
		t.Errorf("redacted network error is no longer retryable: %v", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("redacted network error lost its net.Error: %v", err)
	}
}

func TestTransportErrorKeepsDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	cfg := &Config{MaxRetries: -1, HTTPClient: &http.Client{Timeout: 20 * time.Millisecond}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	_, err := cfg.Do(context.Background(), requestTo(srv.URL+"?key="+testSecret))
	if err == nil {
		t.Fatal("want a timeout")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("timeout error leaks the key: %v", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("err = %v, want a net.Error timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want errors.Is context.DeadlineExceeded", err)
	}
}

func TestBuildErrorRedactsURL(t *testing.T) {
	cfg := testConfig(t)
	_, err := cfg.Do(context.Background(), requestTo("http://h/\x7f?key="+testSecret))
	if err == nil {
		t.Fatal("want a build error")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("build error leaks the key: %v", err)
	}
}

func TestAPIErrorRedactsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid api key `+testSecret+`"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.APIKey = testSecret
	_, err := cfg.Do(context.Background(), requestTo(srv.URL))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if strings.Contains(apiErr.Error(), testSecret) || strings.Contains(apiErr.Body, testSecret) {
		t.Errorf("error echoes the key: %v", apiErr)
	}
	if !strings.Contains(apiErr.Body, "[REDACTED]") {
		t.Errorf("Body = %q, want the key replaced", apiErr.Body)
	}
	if apiErr.Kind() != ErrorAuth {
		t.Errorf("Kind = %q, want %q", apiErr.Kind(), ErrorAuth)
	}
}

func TestAPIErrorShortKeyNotRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the request was bad", http.StatusBadRequest)
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.APIKey = "the" // too short to redact without mangling the body
	_, err := cfg.Do(context.Background(), requestTo(srv.URL))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Body, "the request was bad") {
		t.Fatalf("err = %v, want the body unchanged", err)
	}
}

func TestAPIErrorKind(t *testing.T) {
	tests := []struct {
		name string
		err  *APIError
		want ErrorKind
	}{
		{"429", &APIError{Status: 429}, ErrorRateLimit},
		{"401", &APIError{Status: 401}, ErrorAuth},
		{"403", &APIError{Status: 403}, ErrorAuth},
		{"529", &APIError{Status: 529}, ErrorOverloaded},
		{"503", &APIError{Status: 503}, ErrorOverloaded},
		{"400", &APIError{Status: 400, Body: "bad field"}, ErrorInvalidRequest},
		{"404", &APIError{Status: 404}, ErrorInvalidRequest},
		{"422", &APIError{Status: 422}, ErrorInvalidRequest},
		{"400 overflow", &APIError{Status: 400, Body: "prompt is too long"}, ErrorContextOverflow},
		{"413 overflow", &APIError{Status: 413, Body: "context_length_exceeded"}, ErrorContextOverflow},
		{"500", &APIError{Status: 500}, ErrorServer},
		{"502", &APIError{Status: 502}, ErrorServer},
		{"504", &APIError{Status: 504}, ErrorServer},
		{"409", &APIError{Status: 409}, ErrorUnknown},
		{"stream overloaded", &APIError{Body: `{"type":"error","error":{"type":"overloaded_error"}}`}, ErrorOverloaded},
		{"stream rate limit", &APIError{Body: `{"type":"error","error":{"type":"rate_limit_error"}}`}, ErrorRateLimit},
		{"stream responses rate limit", &APIError{Body: `{"error":{"code":"rate_limit_exceeded"}}`}, ErrorRateLimit},
		{"stream api error", &APIError{Body: `{"type":"error","error":{"type":"api_error"}}`}, ErrorServer},
		{"stream server error", &APIError{Body: `{"error":{"type":"server_error"}}`}, ErrorServer},
		{"stream overflow", &APIError{Body: `{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`}, ErrorContextOverflow},
		{"stream other", &APIError{Body: `{"error":{"type":"invalid_request_error"}}`}, ErrorUnknown},
		{"stream not json", &APIError{Body: "boom"}, ErrorUnknown},
	}
	for _, tt := range tests {
		if got := tt.err.Kind(); got != tt.want {
			t.Errorf("%s: Kind = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestAPIErrorRequestID(t *testing.T) {
	for _, header := range []string{"x-request-id", "request-id"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(header, "req_123")
			w.Header().Set("X-Other", "kept")
			w.WriteHeader(http.StatusBadRequest)
		}))
		_, err := testConfig(t).Do(context.Background(), requestTo(srv.URL))
		srv.Close()
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%s: err = %v, want *APIError", header, err)
		}
		if apiErr.RequestID != "req_123" {
			t.Errorf("%s: RequestID = %q", header, apiErr.RequestID)
		}
		if apiErr.Header.Get("X-Other") != "kept" {
			t.Errorf("%s: Header = %v", header, apiErr.Header)
		}
		if !strings.Contains(apiErr.Error(), "req_123") {
			t.Errorf("%s: Error() = %q, want the request id", header, apiErr.Error())
		}
	}
}

func TestEndpoint(t *testing.T) {
	tests := []struct{ base, path, want string }{
		{"https://h/v1", "chat/completions", "https://h/v1/chat/completions"},
		{"https://h/v1/", "chat/completions", "https://h/v1/chat/completions"},
		{"https://h/v1?key=k", "chat/completions", "https://h/v1/chat/completions?key=k"},
		{"https://h", "v1/messages", "https://h/v1/messages"},
		{"https://h/?a=1&b=2", "responses", "https://h/responses?a=1&b=2"},
	}
	for _, tt := range tests {
		got, err := endpoint(tt.base, tt.path)
		if err != nil || got != tt.want {
			t.Errorf("endpoint(%q, %q) = %q, %v; want %q", tt.base, tt.path, got, err, tt.want)
		}
	}
	_, err := endpoint("https://h/\x7f?key="+testSecret, "x")
	if err == nil || strings.Contains(err.Error(), testSecret) {
		t.Errorf("bad base error = %v, want an error without the key", err)
	}
}

func TestVersionedEndpoints(t *testing.T) {
	anth := &anthropicProvider{cfg: &Config{BaseURL: "https://h?key=k"}}
	if got, _ := anth.messagesURL(); got != "https://h/v1/messages?key=k" {
		t.Errorf("anthropic URL = %q", got)
	}
	anth.cfg.BaseURL = "https://h/v1/?key=k"
	if got, _ := anth.messagesURL(); got != "https://h/v1/messages?key=k" {
		t.Errorf("anthropic /v1 URL = %q", got)
	}
	s1 := &systemOne{cfg: &Config{BaseURL: "https://h/v1?key=k"}}
	if got, _ := s1.url(); got != "https://h/v1/systemone?key=k" {
		t.Errorf("systemone URL = %q", got)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://user:pass@h/v1?key=" + testSecret + "&key=two&x=y#frag")
	for _, leak := range []string{testSecret, "two", "user", "pass", "=y"} {
		if strings.Contains(got, leak) {
			t.Errorf("redactURL leaks %q: %s", leak, got)
		}
	}
	u, err := url.Parse(got)
	if err != nil || u.Host != "h" || u.Path != "/v1" || u.Query().Get("key") != "REDACTED" {
		t.Errorf("redactURL = %q", got)
	}
	if got := redactURL("https://h/v1"); got != "https://h/v1" {
		t.Errorf("redactURL changed a clean URL: %q", got)
	}
	if got := redactURL("http://h/\x7f?key=" + testSecret); strings.Contains(got, testSecret) {
		t.Errorf("redactURL leaks from an unparsable URL: %q", got)
	}
}
