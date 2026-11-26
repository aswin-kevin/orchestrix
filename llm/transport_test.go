package llm_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
)

// getTo returns a build func for a GET of url.
func getTo(url string) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, url, nil)
	}
}

// newTransport builds a Transport from cfg or fails the test.
func newTransport(t *testing.T, cfg llm.Config) *llm.Transport {
	t.Helper()
	tr, err := llm.NewTransport(cfg)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	return tr
}

// TestTransportDoRetriesOutsidePackage checks that an adapter outside
// package llm gets the shared retry behaviour through Transport.Do.
func TestTransportDoRetriesOutsidePackage(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tr := newTransport(t, llm.Config{RetryInterval: time.Millisecond})
	resp, err := tr.Do(t.Context(), getTo(srv.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("got %d %q, want 200 ok", resp.StatusCode, body)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hit %d times, want 2", got)
	}
}

// blockingSSE answers with an event stream holding first, then sends
// nothing more until block passes or the client goes away.
func blockingSSE(t *testing.T, contentType, first string, block time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(block):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTransportStreamIdleOutsidePackage(t *testing.T) {
	url := blockingSSE(t, "text/event-stream", "data: a\n\n", 2*time.Second)
	tr := newTransport(t, llm.Config{StreamIdleTimeout: 100 * time.Millisecond, MaxRetries: llm.NoRetries})

	next, stop := iter.Pull2(tr.Stream(t.Context(), getTo(url)))
	defer stop()
	ev, err, ok := next()
	if !ok || err != nil || ev != (llm.SSEEvent{Data: "a"}) {
		t.Fatalf("first item = %+v, %v, %v; want the event a", ev, err, ok)
	}
	_, err, ok = next()
	if !ok || !errors.Is(err, llm.ErrStreamIdle) {
		t.Fatalf("second item = %v, %v; want ErrStreamIdle", err, ok)
	}
	if _, _, ok := next(); ok {
		t.Error("the sequence went on after its error")
	}
}

func TestTransportStreamPausesWhileConsumerWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: 1\n\ndata: 2\n\ndata: 3\n\n")
	}))
	defer srv.Close()
	tr := newTransport(t, llm.Config{StreamIdleTimeout: 100 * time.Millisecond, MaxRetries: llm.NoRetries})

	var got []string
	for ev, err := range tr.Stream(t.Context(), getTo(srv.URL)) {
		if err != nil {
			t.Fatalf("Stream error: %v", err)
		}
		got = append(got, ev.Data)
		time.Sleep(300 * time.Millisecond)
	}
	if strings.Join(got, ",") != "1,2,3" {
		t.Errorf("events = %v, want 1,2,3", got)
	}
}

func TestTransportStreamDoErrorYieldedOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()
	tr := newTransport(t, llm.Config{MaxRetries: llm.NoRetries})

	var errs []error
	for _, err := range tr.Stream(t.Context(), getTo(srv.URL)) {
		errs = append(errs, err)
	}
	var apiErr *llm.APIError
	if len(errs) != 1 || !errors.As(errs[0], &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("items = %v, want one 400 APIError", errs)
	}
}

func TestTransportFetchIdle(t *testing.T) {
	url := blockingSSE(t, "application/json", `{"id":"m","content":[`, 2*time.Second)
	tr := newTransport(t, llm.Config{StreamIdleTimeout: 100 * time.Millisecond, MaxRetries: llm.NoRetries})
	start := time.Now()
	if _, err := tr.Fetch(t.Context(), getTo(url)); !errors.Is(err, llm.ErrStreamIdle) {
		t.Fatalf("err = %v, want ErrStreamIdle", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("stalled read ended after %v, want within 1s", elapsed)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	body, err := tr.Fetch(t.Context(), getTo(srv.URL))
	if err != nil || string(body) != `{"ok":true}` {
		t.Errorf("Fetch = %q, %v; want the body", body, err)
	}
}

func TestTransportRedact(t *testing.T) {
	const key = "sk-secret-123456"
	tr := newTransport(t, llm.Config{APIKey: key})

	orig := fmt.Errorf("bad %s", key)
	redacted := tr.Redact(orig)
	if text := redacted.Error(); strings.Contains(text, key) || !strings.Contains(text, "[REDACTED]") {
		t.Errorf("Redact text = %q, want the key replaced", text)
	}
	if !errors.Is(redacted, orig) {
		t.Error("Redact lost the original error")
	}
	if tr.Redact(nil) != nil {
		t.Error("Redact(nil) != nil")
	}

	apiErr := tr.StreamError(`{"k":"` + key + `"}`)
	if apiErr.Status != 0 || strings.Contains(apiErr.Body, key) || !strings.Contains(apiErr.Body, "[REDACTED]") {
		t.Errorf("StreamError = %+v, want Status 0 and a redacted Body", apiErr)
	}
}

func TestNewTransportBadProxy(t *testing.T) {
	tr, err := llm.NewTransport(llm.Config{Proxy: "://bad"})
	if tr != nil || err == nil || !strings.Contains(err.Error(), "parse proxy URL") {
		t.Fatalf("NewTransport = %v, %v; want a parse proxy URL error", tr, err)
	}
}

func TestTransportConfigIsCopy(t *testing.T) {
	headers := map[string]string{"X-A": "1"}
	tr := newTransport(t, llm.Config{Model: "m", Headers: headers})
	headers["X-A"] = "changed"
	headers["X-B"] = "added"
	got := tr.Config()
	if got.Model != "m" || len(got.Headers) != 1 || got.Headers["X-A"] != "1" {
		t.Fatalf("Config() = %+v, want the headers as given", got)
	}
	got.Headers["X-A"] = "mutated"
	got.Model = "other"
	if again := tr.Config(); again.Headers["X-A"] != "1" || again.Model != "m" {
		t.Errorf("second Config() = %+v, want it unchanged", again)
	}
}

// pendingRecorder is a provider whose Stream records what FallbackPending
// reports for a 503 in its call's context, then fails with that 503.
type pendingRecorder struct {
	mu  sync.Mutex
	got []bool
}

func (p *pendingRecorder) Stream(ctx context.Context, _ llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		err := &llm.APIError{Status: http.StatusServiceUnavailable}
		p.mu.Lock()
		p.got = append(p.got, llm.FallbackPending(ctx, err))
		p.mu.Unlock()
		yield(llm.StreamEvent{}, err)
	}
}

func (p *pendingRecorder) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, p, req)
}

func (p *pendingRecorder) recorded() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.got...)
}

func TestFallbackPending(t *testing.T) {
	if llm.FallbackPending(t.Context(), &llm.APIError{Status: http.StatusServiceUnavailable}) {
		t.Error("FallbackPending outside a fallback = true, want false")
	}

	first, last := &pendingRecorder{}, &pendingRecorder{}
	f, err := llm.NewFallback(llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Name: "first", Provider: first},
		{Name: "last", Provider: last},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range f.Stream(t.Context(), llm.Request{Messages: []llm.Message{llm.UserText("x")}}) {
	}
	if got := first.recorded(); len(got) != 1 || !got[0] {
		t.Errorf("first entry recorded %v, want [true]", got)
	}
	if got := last.recorded(); len(got) != 1 || got[0] {
		t.Errorf("last entry recorded %v, want [false]", got)
	}
}
