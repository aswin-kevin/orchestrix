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

// testTransport returns a Transport with a tiny fixed retry wait, after
// applying mutate to its Config.
func testTransport(t *testing.T, mutate ...func(*Config)) *Transport {
	t.Helper()
	cfg := Config{RetryInterval: time.Millisecond}
	for _, m := range mutate {
		m(&cfg)
	}
	tr, err := NewTransport(cfg)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	return tr
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

	resp, err := testTransport(t).Do(context.Background(), requestTo(srv.URL))
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

	_, err := testTransport(t).Do(context.Background(), requestTo(srv.URL))
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

	tr := testTransport(t, func(c *Config) { c.MaxRetries = -1 })
	if _, err := tr.Do(context.Background(), requestTo(srv.URL)); err == nil {
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

	tr := testTransport(t, func(c *Config) { c.MaxRetries = 2 })
	if _, err := tr.Do(context.Background(), requestTo(srv.URL)); !IsRetryable(err) {
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
	_, err := testTransport(t).Do(ctx, requestTo(srv.URL))
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

	tr := testTransport(t, func(c *Config) { c.RetryInterval = time.Minute })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err := tr.Do(ctx, requestTo(srv.URL))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestDoSkipsWaitPastDeadline checks that a retry wait that would end after
// the context's deadline is not started: the provider's error comes back at
// once, while the context is still live, instead of a deadline error later.
func TestDoSkipsWaitPastDeadline(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "20")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := testTransport(t).Do(ctx, requestTo(srv.URL))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want the 429 APIError", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("returned after %v, want at once", elapsed)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
}

// TestDoHandsOffToFallback checks that a call made for a fallback entry that
// is not the last does not retry an error the fallback would switch on.
func TestDoHandsOffToFallback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx := withHandoff(context.Background(), DefaultShouldFallback)
	if _, err := testTransport(t).Do(ctx, requestTo(srv.URL)); err == nil {
		t.Fatal("want an error")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1 (no retries before handing off)", got)
	}

	// An error the fallback would not switch on is retried as usual.
	hits.Store(0)
	ctx = withHandoff(context.Background(), func(error) bool { return false })
	if _, err := testTransport(t).Do(ctx, requestTo(srv.URL)); err == nil {
		t.Fatal("want an error")
	}
	if got, want := hits.Load(), int32(DefaultMaxRetries+1); got != want {
		t.Errorf("hits = %d, want %d", got, want)
	}
}

func TestRetryDelay(t *testing.T) {
	tr := &Transport{}
	rateLimited := &APIError{Status: 429, RetryAfter: 3 * time.Second, hasRetryAfter: true}
	plain := &APIError{Status: 500}

	// Retry-After is honoured and only ever lengthened, by at most a tenth.
	for range 100 {
		if got := tr.retryDelay(0, rateLimited); got < 3*time.Second || got > 3300*time.Millisecond {
			t.Fatalf("Retry-After delay = %v, want within [3s, 3.3s]", got)
		}
	}
	for attempt, ceiling := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second} {
		if got := backoff(attempt); got != ceiling {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, ceiling)
		}
		for range 100 {
			if got := tr.retryDelay(attempt, plain); got < ceiling/2 || got > ceiling {
				t.Fatalf("retryDelay(%d) = %v, want within [%v, %v]", attempt, got, ceiling/2, ceiling)
			}
		}
	}
	tr.cfg.RetryInterval = 40 * time.Millisecond
	if got := tr.retryDelay(5, plain); got != 40*time.Millisecond {
		t.Errorf("fixed interval delay = %v", got)
	}

	// A huge Retry-After is capped at maxRetryAfter, plus its jitter.
	huge := &APIError{Status: 429, RetryAfter: time.Hour, hasRetryAfter: true}
	for range 100 {
		if got := tr.retryDelay(0, huge); got < maxRetryAfter || got > maxRetryAfter+maxRetryAfter/retryAfterJitterDivisor {
			t.Fatalf("capped Retry-After delay = %v, want within [%v, %v]", got, maxRetryAfter, maxRetryAfter+maxRetryAfter/retryAfterJitterDivisor)
		}
	}
}

// TestRetryDelaySpreadsCallers checks that callers failing together do not
// all wait the same time before retrying.
func TestRetryDelaySpreadsCallers(t *testing.T) {
	tr := &Transport{}
	rateLimited := &APIError{Status: 429, RetryAfter: 3 * time.Second, hasRetryAfter: true}
	for _, err := range []error{&APIError{Status: 500}, rateLimited} {
		seen := make(map[time.Duration]bool)
		for range 20 {
			seen[tr.retryDelay(2, err)] = true
		}
		if len(seen) < 2 {
			t.Errorf("retryDelay(2, %v) returned the same delay 20 times", err)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		delay time.Duration
		ok    bool
	}{
		{"", 0, false},
		{"junk", 0, false},
		{"-3", 0, false},
		{"0", 0, true},
		{"7", 7 * time.Second, true},
		{"Thu, 01 Jan 2026 12:00:30 GMT", 30 * time.Second, true},
		{"Wed, 31 Dec 2025 12:00:00 GMT", 0, true},
	}
	for _, tt := range tests {
		delay, ok := parseRetryAfter(tt.value, now)
		if delay != tt.delay || ok != tt.ok {
			t.Errorf("parseRetryAfter(%q) = %v, %v; want %v, %v", tt.value, delay, ok, tt.delay, tt.ok)
		}
	}
}

func TestDoBuildErrorIsNotRetried(t *testing.T) {
	var builds int
	tr := testTransport(t)
	_, err := tr.Do(context.Background(), func() (*http.Request, error) {
		builds++
		return http.NewRequest(http.MethodPost, "http://bad host/", nil)
	})
	if err == nil {
		t.Fatal("want error")
	}
	if builds != 1 {
		t.Errorf("builds = %d, want 1", builds)
	}
	if IsRetryable(err) {
		t.Errorf("build error must not be retryable: %v", err)
	}
}

func TestDoRetryAfterZeroRetriesImmediately(t *testing.T) {
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

	// The default backoff would wait 500ms.
	tr, err := NewTransport(Config{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := tr.Do(context.Background(), requestTo(srv.URL))
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("retry took %v, want prompt", elapsed)
	}
}

func TestAPIErrorMessageTruncatesBody(t *testing.T) {
	body := strings.Repeat("x", 5000)
	err := &APIError{Status: 500, Body: body}
	if len(err.Error()) > 700 {
		t.Errorf("message length %d, want truncated", len(err.Error()))
	}
	if err.Body != body {
		t.Error("Body field must stay complete")
	}
	if short := (&APIError{Status: 400, Body: "oops"}).Error(); !strings.Contains(short, "oops") || strings.Contains(short, "truncated") {
		t.Errorf("short body altered: %q", short)
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

	tr := testTransport(t, func(c *Config) { c.MaxRetries = 1 })
	if _, err := tr.Do(context.Background(), requestTo(url)); !IsRetryable(err) {
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
