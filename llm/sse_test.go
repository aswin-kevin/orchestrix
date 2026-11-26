package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/internal/sse"
)

func TestReadSSEParsesEventsAndMultilineData(t *testing.T) {
	input := ": keep-alive\n" +
		"event: a\ndata: x\ndata: y\n\n" +
		"data: {\"n\":1}\r\n\r\n" +
		"event: last\ndata: tail"

	var got []SSEEvent
	for ev, err := range readSSE(strings.NewReader(input), nil) {
		if err != nil {
			t.Fatalf("readSSE error: %v", err)
		}
		got = append(got, ev)
	}

	want := []SSEEvent{
		{Event: "a", Data: "x\ny"},
		{Data: `{"n":1}`},
		{Event: "last", Data: "tail"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReadSSECapsEventSize(t *testing.T) {
	input := "data: " + strings.Repeat("x", maxSSEEventBytes) + "\n\n"
	var err error
	for _, e := range readSSE(strings.NewReader(input), nil) {
		if e != nil {
			err = e
			break
		}
	}
	if !errors.Is(err, sse.ErrTooLarge) || !strings.HasPrefix(err.Error(), "llm: read event stream:") {
		t.Fatalf("error = %v, want an llm read error wrapping sse.ErrTooLarge", err)
	}
}

// newStallServer answers every request with first and then holds the
// connection open, sending nothing more, until the client goes away. hits
// counts the requests.
func newStallServer(t *testing.T, contentType, first string) (url string, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

const (
	testIdle  = 100 * time.Millisecond
	idleLimit = time.Second
)

// lastStreamError drains a stream and returns its error.
func lastStreamError(ctx context.Context, p Provider) error {
	var last error
	for _, err := range p.Stream(ctx, Request{Messages: []Message{UserText("x")}}) {
		if err != nil {
			last = err
		}
	}
	return last
}

func TestStreamIdleTimeout(t *testing.T) {
	idle := func(c *Config) { c.StreamIdleTimeout = testIdle }
	tests := []struct {
		name  string
		first string
		build func(t *testing.T, url string) Provider
	}{
		{"anthropic", anthropicFirstDelta, func(t *testing.T, url string) Provider {
			return newTestProviderWith(t, FormatAnthropic, url, idle)
		}},
		{"openai", openAIFirstDelta, func(t *testing.T, url string) Provider {
			return newTestOpenAI(t, url, idle)
		}},
		{"responses", "data: " + responsesTextDelta + "\n\n", func(t *testing.T, url string) Provider {
			return newTestResponses(t, url, idle)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, _ := newStallServer(t, "text/event-stream", tt.first)
			start := time.Now()
			err := lastStreamError(t.Context(), tt.build(t, url))
			if !errors.Is(err, ErrStreamIdle) {
				t.Fatalf("err = %v, want ErrStreamIdle", err)
			}
			if elapsed := time.Since(start); elapsed > idleLimit {
				t.Errorf("idle stream ended after %v, want within %v", elapsed, idleLimit)
			}
		})
	}
}

func TestStreamIdleTimeoutBeforeFirstEvent(t *testing.T) {
	url, _ := newStallServer(t, "text/event-stream", "")
	err := lastStreamError(t.Context(), newTestOpenAI(t, url, func(c *Config) { c.StreamIdleTimeout = testIdle }))
	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("err = %v, want ErrStreamIdle", err)
	}
}

func TestIdleTimeoutAfterContentNotRetried(t *testing.T) {
	url, hits := newStallServer(t, "text/event-stream", openAIFirstDelta)
	p := newTestOpenAI(t, url, func(c *Config) {
		c.StreamIdleTimeout = testIdle
		c.MaxRetries = 3
	})

	var text strings.Builder
	var last error
	for ev, err := range p.Stream(t.Context(), Request{Messages: []Message{UserText("x")}}) {
		if err != nil {
			last = err
			continue
		}
		text.WriteString(ev.Text)
	}
	if !errors.Is(last, ErrStreamIdle) {
		t.Fatalf("err = %v, want ErrStreamIdle", last)
	}
	if text.String() != "a" {
		t.Errorf("text = %q, want the delivered content once", text.String())
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hit %d times, want 1", got)
	}
}

func TestCallerDeadlineBeatsIdleTimeout(t *testing.T) {
	url, _ := newStallServer(t, "text/event-stream", openAIFirstDelta)
	p := newTestOpenAI(t, url, func(c *Config) { c.StreamIdleTimeout = 10 * time.Second })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if err := lastStreamError(ctx, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestStreamIdleIsRetryable(t *testing.T) {
	if !IsRetryable(ErrStreamIdle) || !DefaultShouldFallback(ErrStreamIdle) {
		t.Error("ErrStreamIdle should be retryable and switch a fallback chain")
	}
}

func TestFallbackSwitchesOnIdleBeforeContent(t *testing.T) {
	stalled, _ := newStallServer(t, "text/event-stream", "")
	healthy := newSSEServer(t, 200, "text/event-stream", openAIHelloStream)
	f, err := NewFallback(FallbackConfig{Entries: []FallbackEntry{
		{Name: "stalled", Provider: newTestOpenAI(t, stalled, func(c *Config) { c.StreamIdleTimeout = testIdle })},
		{Name: "healthy", Provider: newTestOpenAI(t, healthy.URL)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := Collect(t.Context(), f, Request{Messages: []Message{UserText("x")}})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Message.Text() != "hi" {
		t.Errorf("text = %q, want the healthy entry's reply", resp.Message.Text())
	}
}

func TestAnthropicCompleteIdleTimeout(t *testing.T) {
	url, _ := newStallServer(t, "application/json", `{"id":"m","content":[`)
	p := newTestProviderWith(t, FormatAnthropic, url, func(c *Config) { c.StreamIdleTimeout = testIdle })
	start := time.Now()
	_, err := p.Complete(t.Context(), Request{Messages: []Message{UserText("x")}})
	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("err = %v, want ErrStreamIdle", err)
	}
	if elapsed := time.Since(start); elapsed > idleLimit {
		t.Errorf("stalled body read ended after %v, want within %v", elapsed, idleLimit)
	}
}

func TestStreamIdleTimeoutDefault(t *testing.T) {
	for _, tt := range []struct {
		set, want time.Duration
	}{{0, DefaultStreamIdleTimeout}, {-1, 0}, {time.Second, time.Second}} {
		tr := testTransport(t, func(c *Config) { c.StreamIdleTimeout = tt.set })
		if got := tr.streamIdleTimeout(); got != tt.want {
			t.Errorf("streamIdleTimeout(%v) = %v, want %v", tt.set, got, tt.want)
		}
	}
}

// newTestProviderWith builds a provider of format f against url without
// retries, after applying mutate to its Config.
func newTestProviderWith(t *testing.T, f Format, url string, mutate func(*Config)) Provider {
	t.Helper()
	cfg := Config{Format: f, BaseURL: url, APIKey: "key-1", Model: "test-model", MaxRetries: -1}
	mutate(&cfg)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStreamKeepAliveCommentsResetIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for range 10 {
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			flusher.Flush()
			select {
			case <-time.After(30 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, openAIHelloStream)
	}))
	t.Cleanup(srv.Close)
	p := newTestOpenAI(t, srv.URL, func(c *Config) { c.StreamIdleTimeout = testIdle })

	resp, err := Collect(t.Context(), p, Request{Messages: []Message{UserText("x")}})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Message.Text() != "hi" {
		t.Errorf("text = %q, want hi", resp.Message.Text())
	}
}

func TestCallerCancelWithIdleTimerIsCanceled(t *testing.T) {
	url, _ := newStallServer(t, "text/event-stream", openAIFirstDelta)
	p := newTestOpenAI(t, url, func(c *Config) { c.StreamIdleTimeout = 10 * time.Second })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var last error
	for _, err := range p.Stream(ctx, Request{Messages: []Message{UserText("x")}}) {
		if err == nil {
			cancel()
		}
		last = err
	}
	if !errors.Is(last, context.Canceled) || errors.Is(last, ErrStreamIdle) {
		t.Fatalf("err = %v, want context.Canceled and not ErrStreamIdle", last)
	}
}
