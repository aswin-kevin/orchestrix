package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testConfig returns a prepared Config with a tiny fixed retry wait.
func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{RetryInterval: time.Millisecond}
	if err := cfg.prepare(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return cfg
}

func requestTo(url string) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	}
}

func TestDoRetries429ThenSucceeds(t *testing.T) {
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

	resp, err := testConfig(t).do(context.Background(), requestTo(srv.URL))
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Errorf("hits = %d, want 2", got)
	}
}

func TestDoDoesNotRetry400(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "bad request body", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := testConfig(t).do(context.Background(), requestTo(srv.URL))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want *APIError with status 400", err)
	}
	if !strings.Contains(apiErr.Body, "bad request body") {
		t.Errorf("Body = %q", apiErr.Body)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
}

func TestDoHonorsMaxRetriesNegative(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.MaxRetries = -1
	if _, err := cfg.do(context.Background(), requestTo(srv.URL)); err == nil {
		t.Fatal("want error")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
}

func TestDoGivesUpAfterMaxRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(statusOverloaded)
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.MaxRetries = 2
	if _, err := cfg.do(context.Background(), requestTo(srv.URL)); !IsRetryable(err) {
		t.Fatalf("err = %v, want the last retryable APIError", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("hits = %d, want 3", got)
	}
}

func TestDoStopsOnContextCancel(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := testConfig(t).do(ctx, requestTo(srv.URL))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Error("do did not return promptly")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("hits = %d, want 0", got)
	}
}

func TestDoCancelDuringRetryWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.RetryInterval = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := cfg.do(ctx, requestTo(srv.URL))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestRetryDelay(t *testing.T) {
	cfg := &Config{}
	rateLimited := &APIError{Status: 429, RetryAfter: 3 * time.Second}
	plain := &APIError{Status: 500}

	if got := cfg.retryDelay(0, rateLimited); got != 3*time.Second {
		t.Errorf("Retry-After delay = %v", got)
	}
	for attempt, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second} {
		if got := cfg.retryDelay(attempt, plain); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
	cfg.RetryInterval = 40 * time.Millisecond
	if got := cfg.retryDelay(5, plain); got != 40*time.Millisecond {
		t.Errorf("fixed interval delay = %v", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		"-3":                            0,
		"junk":                          0,
		"Thu, 01 Jan 2026 12:00:30 GMT": 30 * time.Second,
		"Wed, 31 Dec 2025 12:00:00 GMT": 0,
	}
	for value, want := range tests {
		if got := parseRetryAfter(value, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestIsContextOverflow(t *testing.T) {
	bodies := []string{
		"prompt is too long: 250000 tokens",
		`{"code":"context_length_exceeded"}`,
		"This model's Maximum Context Length is 8192",
		"exceeds the context window",
		"too many tokens",
		"Input is too long for requested model",
	}
	for _, body := range bodies {
		for _, status := range []int{400, 413} {
			if !IsContextOverflow(&APIError{Status: status, Body: body}) {
				t.Errorf("status %d body %q: want overflow", status, body)
			}
		}
	}
	if IsContextOverflow(&APIError{Status: 429, Body: "prompt is too long"}) {
		t.Error("429 must not be overflow")
	}
	if IsContextOverflow(&APIError{Status: 400, Body: "invalid json"}) {
		t.Error("unrelated 400 must not be overflow")
	}
	if IsContextOverflow(errors.New("prompt is too long")) {
		t.Error("non-APIError must not be overflow")
	}
}

func TestIsRetryable(t *testing.T) {
	for _, status := range []int{408, 409, 429, 500, 502, 529} {
		if !IsRetryable(&APIError{Status: status}) {
			t.Errorf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 413} {
		if IsRetryable(&APIError{Status: status}) {
			t.Errorf("status %d should not be retryable", status)
		}
	}
	if IsRetryable(context.Canceled) || IsRetryable(context.DeadlineExceeded) {
		t.Error("context errors must not be retryable")
	}
}

func TestDoRetriesNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // connections to url are now refused

	cfg := testConfig(t)
	cfg.MaxRetries = 1
	if _, err := cfg.do(context.Background(), requestTo(url)); !IsRetryable(err) {
		t.Fatalf("err = %v, want a retryable network error", err)
	}
}

func TestRateLimitPerSecond(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	l := newLimiter(&RateLimit{PerSecond: 2})
	l.now, l.sleep = clock.Now, clock.Sleep

	start := clock.Now()
	for range 3 {
		if err := l.acquire(context.Background()); err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	if got := clock.Now().Sub(start); got != 500*time.Millisecond {
		t.Errorf("3 acquisitions at 2/s took %v, want 500ms", got)
	}
}

func TestRateLimitPerMinuteAndNil(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	l := newLimiter(&RateLimit{PerMinute: 2})
	l.now, l.sleep = clock.Now, clock.Sleep

	start := clock.Now()
	for range 3 {
		_ = l.acquire(context.Background())
	}
	if got := clock.Now().Sub(start); got != 30*time.Second {
		t.Errorf("took %v, want 30s", got)
	}

	if newLimiter(nil) != nil || newLimiter(&RateLimit{}) != nil {
		t.Error("empty RateLimit should yield no limiter")
	}
	var none *limiter
	if err := none.acquire(context.Background()); err != nil {
		t.Errorf("nil limiter acquire: %v", err)
	}
}

func TestRateLimitRespectsContext(t *testing.T) {
	l := newLimiter(&RateLimit{PerMinute: 1})
	_ = l.acquire(context.Background()) // consumes the only token

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestRateLimitConcurrentUse(t *testing.T) {
	var mu sync.Mutex
	var longest time.Duration
	l := newLimiter(&RateLimit{PerSecond: 10})
	l.now = func() time.Time { return time.Unix(1000, 0) }
	l.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		longest = max(longest, d)
		return nil
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.acquire(context.Background())
		}()
	}
	wg.Wait()
	// 20 simultaneous requests at 10/s: a burst of 10, then the last waits 1s.
	if longest != time.Second {
		t.Errorf("longest wait %v, want 1s", longest)
	}
}

// fakeClock is a virtual clock whose Sleep advances time instead of waiting.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(max(d, 0))
	return nil
}
