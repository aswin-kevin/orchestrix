package llm

import (
	"context"
	"errors"
	"fmt"
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
		// Echo the request URI, as some gateways do, so the body carries the
		// query key.
		http.Error(w, "upstream failed for "+r.URL.RequestURI(), http.StatusInternalServerError)
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

	tr, err := NewTransport(Config{MaxRetries: -1, HTTPClient: &http.Client{Timeout: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Do(context.Background(), requestTo(srv.URL+"?key="+testSecret))
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
	tr := testTransport(t)
	_, err := tr.Do(context.Background(), requestTo("http://h/\x7f?key="+testSecret))
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

	tr := testTransport(t, func(c *Config) { c.APIKey = testSecret })
	_, err := tr.Do(context.Background(), requestTo(srv.URL))
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

	// "the" is too short to redact without mangling the body.
	tr := testTransport(t, func(c *Config) { c.APIKey = "the" })
	_, err := tr.Do(context.Background(), requestTo(srv.URL))
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
		_, err := testTransport(t).Do(context.Background(), requestTo(srv.URL))
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
	anth := &anthropicProvider{cfg: Config{BaseURL: "https://h?key=k"}}
	if got, err := anth.messagesURL(); err != nil || got != "https://h/v1/messages?key=k" {
		t.Errorf("anthropic URL = %q, %v", got, err)
	}
	anth.cfg.BaseURL = "https://h/v1/?key=k"
	if got, err := anth.messagesURL(); err != nil || got != "https://h/v1/messages?key=k" {
		t.Errorf("anthropic /v1 URL = %q, %v", got, err)
	}
	s1 := &systemOne{cfg: Config{BaseURL: "https://h/v1?key=k"}}
	if got, err := s1.url(); err != nil || got != "https://h/v1/systemone?key=k" {
		t.Errorf("systemone URL = %q, %v", got, err)
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

// secretConfig returns a Config whose API key, sensitive headers and BaseURL
// password all hold distinct secrets.
func secretConfig(t *testing.T, base string) (Config, []string) {
	t.Helper()
	secrets := []string{"SECRET-apikey-1", "SECRET-bearer-2", "SECRET-header-3", "SECRET-token-4", "SECRET-password-5"}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("user", secrets[4])
	cfg := Config{
		BaseURL:    u.String(),
		APIKey:     secrets[0],
		MaxRetries: -1,
		Headers: map[string]string{
			"Proxy-Authorization": "Bearer " + secrets[1],
			"X-Goog-Api-Key":      secrets[2],
			"X-Session-Token":     secrets[3],
			"X-Trace":             "not-a-secret-value",
		},
	}
	return cfg, secrets
}

func TestAPIErrorRedactsConfiguredSecrets(t *testing.T) {
	var echo atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, echo.Load().(string), http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg, secrets := secretConfig(t, srv.URL)
	echo.Store("got " + strings.Join(secrets, " ") + " and not-a-secret-value")
	tr, err := NewTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Do(context.Background(), requestTo(srv.URL))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	for _, secret := range secrets {
		if strings.Contains(apiErr.Body, secret) || strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %v", secret, err)
		}
	}
	if !strings.Contains(apiErr.Body, "not-a-secret-value") {
		t.Errorf("Body = %q, an ordinary header value was redacted", apiErr.Body)
	}
}

func TestTransportErrorRedactsConfiguredSecrets(t *testing.T) {
	cfg, secrets := secretConfig(t, "http://h")
	failing := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("proxy said: " + strings.Join(secrets, " "))
	})}
	cfg.HTTPClient = failing
	tr, err := NewTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Do(context.Background(), requestTo("http://h/x"))
	if err == nil {
		t.Fatal("want a transport error")
	}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("transport error leaks %q: %v", secret, err)
		}
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || !IsRetryable(err) {
		t.Errorf("err = %v, want a retryable *url.Error underneath", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStreamErrorRedactsConfiguredSecrets(t *testing.T) {
	const key = "SECRET-apikey-1"
	tests := []struct {
		name string
		body string
		run  func(t *testing.T, base string) error
	}{
		{
			name: "anthropic",
			body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"bad " + key + "\"}}\n\n",
			run: func(t *testing.T, base string) error {
				p, err := newAnthropic(Config{BaseURL: base, APIKey: key, Model: "m", MaxRetries: -1})
				if err != nil {
					t.Fatal(err)
				}
				_, err = Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}})
				return err
			},
		},
		{
			name: "openai chat",
			body: chunks(`{"error":{"message":"bad ` + key + `"}}`),
			run: func(t *testing.T, base string) error {
				p := newTestOpenAI(t, base, func(c *Config) { c.APIKey = key })
				_, err := Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}})
				return err
			},
		},
		{
			name: "responses",
			body: responsesBody(`{"type":"error","code":"invalid_api_key","message":"bad ` + key + `"}`),
			run: func(t *testing.T, base string) error {
				p := newTestResponses(t, base, func(c *Config) { c.APIKey = key })
				_, err := Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newSSEServer(t, 200, "text/event-stream", tt.body)
			err := tt.run(t, srv.URL)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 0 {
				t.Fatalf("err = %v, want an in-stream *APIError", err)
			}
			if strings.Contains(apiErr.Body, key) || strings.Contains(err.Error(), key) {
				t.Errorf("stream error leaks the key: %v", err)
			}
			if !strings.Contains(apiErr.Body, "[REDACTED]") {
				t.Errorf("Body = %q, want the key replaced", apiErr.Body)
			}
		})
	}
}

func TestAPIErrorHeaderDropsCookies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "session=abc")
		w.Header().Add("Set-Cookie2", "old=def")
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	tr := testTransport(t, func(c *Config) { c.MaxRetries = -1 })
	_, err := tr.Do(context.Background(), requestTo(srv.URL))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Header.Get("Set-Cookie") != "" || apiErr.Header.Get("Set-Cookie2") != "" {
		t.Errorf("Header keeps cookies: %v", apiErr.Header)
	}
	if apiErr.Header.Get("X-Ratelimit-Remaining") != "0" {
		t.Errorf("Header = %v, want other headers kept", apiErr.Header)
	}
}

type flaky struct{}

func (flaky) Error() string   { return "flaky" }
func (flaky) Retryable() bool { return true }

type final struct{ err error }

func (f final) Error() string   { return "final" }
func (f final) Unwrap() error   { return f.err }
func (f final) Retryable() bool { return false }

type tooBig struct{}

func (tooBig) Error() string         { return "too big" }
func (tooBig) ContextOverflow() bool { return true }

func TestRetryableInterface(t *testing.T) {
	err := fmt.Errorf("w: %w", flaky{})
	if !IsRetryable(err) {
		t.Error("IsRetryable = false, want true")
	}
	if !DefaultShouldFallback(err) {
		t.Error("DefaultShouldFallback = false, want true")
	}
}

func TestRetryableInterfaceOverridesStatus(t *testing.T) {
	err := final{err: &APIError{Status: 503}}
	if IsRetryable(err) {
		t.Error("IsRetryable = true, want false: the outer Retryable decides")
	}
}

func TestContextOverflowInterface(t *testing.T) {
	err := fmt.Errorf("w: %w", tooBig{})
	if !IsContextOverflow(err) {
		t.Error("IsContextOverflow = false, want true")
	}
	if DefaultShouldFallback(err) {
		t.Error("DefaultShouldFallback = true, want false")
	}
}

func TestHasRetryAfterExported(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	start := time.Now()
	resp, err := testTransport(t, func(c *Config) { c.RetryInterval = 10 * time.Second }).Do(t.Context(), requestTo(srv.URL))
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Do took %v, want under 1s: Retry-After 0 means retry now", elapsed)
	}

	hits.Store(0)
	_, err = testTransport(t, func(c *Config) { c.MaxRetries = -1 }).Do(t.Context(), requestTo(srv.URL))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if !apiErr.HasRetryAfter || apiErr.RetryAfter != 0 {
		t.Errorf("HasRetryAfter = %v, RetryAfter = %v, want true and 0", apiErr.HasRetryAfter, apiErr.RetryAfter)
	}
}
