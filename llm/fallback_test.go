package llm_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func overloaded() llmtest.Turn { return llmtest.Turn{Err: &llm.APIError{Status: 529}} }

func newFallback(t *testing.T, cfg llm.FallbackConfig) llm.Provider {
	t.Helper()
	p, err := llm.NewFallback(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewFallbackValidates(t *testing.T) {
	ok := llmtest.New()
	cases := map[string][]llm.FallbackEntry{
		"none":      nil,
		"one":       {{Provider: ok}},
		"nil":       {{Provider: ok}, {Provider: nil}},
		"duplicate": {{Provider: ok, Model: "m"}, {Provider: ok, Model: "m"}},
	}
	for name, entries := range cases {
		if _, err := llm.NewFallback(llm.FallbackConfig{Entries: entries}); err == nil {
			t.Errorf("%s: NewFallback succeeded, want an error", name)
		}
	}
	if _, err := llm.NewFallback(llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: ok}, {Provider: ok}}}); err != nil {
		t.Errorf("default names: %v", err)
	}
}

func TestFallbackSwitchesOnOverload(t *testing.T) {
	var events []llm.FallbackEvent
	p := newFallback(t, llm.FallbackConfig{
		Entries: []llm.FallbackEntry{
			{Provider: llmtest.New(overloaded()), Name: "a"},
			{Provider: llmtest.New(llmtest.Turn{Text: "ok"}), Name: "b"},
		},
		OnFallback: func(_ context.Context, ev llm.FallbackEvent) { events = append(events, ev) },
	})
	resp, err := p.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Message.Text(); got != "ok" {
		t.Errorf("text = %q, want ok", got)
	}
	if len(events) != 1 || events[0].From != "a" || events[0].To != "b" {
		t.Fatalf("events = %+v", events)
	}
	var apiErr *llm.APIError
	if !errors.As(events[0].Err, &apiErr) || apiErr.Status != 529 {
		t.Errorf("event err = %v, want status 529", events[0].Err)
	}
}

func TestFallbackClearsModelForBackups(t *testing.T) {
	primary := llmtest.New(overloaded())
	backup := llmtest.New(llmtest.Turn{Text: "ok"})
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: primary}, {Provider: backup}}})
	if _, err := p.Complete(context.Background(), llm.Request{Model: "primary-model"}); err != nil {
		t.Fatal(err)
	}
	if got := primary.Requests[0].Model; got != "primary-model" {
		t.Errorf("primary model = %q", got)
	}
	if got := backup.Requests[0].Model; got != "" {
		t.Errorf("backup model = %q, want empty", got)
	}
}

func TestFallbackClampsMaxTokens(t *testing.T) {
	llm.RegisterProfile("fallback-test-small", llm.Profile{ContextWindow: 100_000, MaxOutput: 1000})
	primary := llmtest.New(overloaded())
	backup := llmtest.New(llmtest.Turn{Text: "ok"})
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: primary},
		{Provider: backup, Model: "fallback-test-small"},
	}})
	req := llm.Request{MaxTokens: 8000}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := primary.Requests[0].MaxTokens; got != 8000 {
		t.Errorf("primary MaxTokens = %d, want 8000", got)
	}
	if got := backup.Requests[0].MaxTokens; got != 1000 {
		t.Errorf("backup MaxTokens = %d, want 1000", got)
	}
	if req.MaxTokens != 8000 {
		t.Errorf("caller's request changed: %d", req.MaxTokens)
	}
}

func TestFallbackCooldown(t *testing.T) {
	primary := llmtest.New(overloaded(), llmtest.Turn{Text: "primary"})
	backup := llmtest.New(llmtest.Turn{Text: "b1"}, llmtest.Turn{Text: "b2"})
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: primary}, {Provider: backup}}})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	llm.SetFallbackClock(p, func() time.Time { return now })

	ctx := context.Background()
	if _, err := p.Complete(ctx, llm.Request{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	resp, err := p.Complete(ctx, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "b2" || primary.Calls() != 1 {
		t.Errorf("call 2: text %q, primary calls %d; want b2 and 1", resp.Message.Text(), primary.Calls())
	}
	now = now.Add(21 * time.Second)
	resp, err = p.Complete(ctx, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "primary" || primary.Calls() != 2 {
		t.Errorf("call 3: text %q, primary calls %d; want primary and 2", resp.Message.Text(), primary.Calls())
	}
}

func TestFallbackAllCoolingStillTries(t *testing.T) {
	primary := llmtest.New(overloaded(), overloaded())
	backup := llmtest.New(overloaded(), llmtest.Turn{Err: &llm.APIError{Status: 503}})
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: primary}, {Provider: backup}}})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	llm.SetFallbackClock(p, func() time.Time { return now })

	ctx := context.Background()
	if _, err := p.Complete(ctx, llm.Request{}); err == nil {
		t.Fatal("call 1 succeeded")
	}
	now = now.Add(time.Second)
	_, err := p.Complete(ctx, llm.Request{})
	if primary.Calls() != 2 || backup.Calls() != 2 {
		t.Errorf("calls = %d, %d; want 2, 2", primary.Calls(), backup.Calls())
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 503 {
		t.Errorf("err = %v, want the backup's 503", err)
	}
}

func TestFallbackNoSwitchOnNonRetryable(t *testing.T) {
	cases := map[string]error{
		"bad request": &llm.APIError{Status: 400},
		"canceled":    fmt.Errorf("send: %w", context.Canceled),
		"overflow":    &llm.APIError{Status: 400, Body: "prompt is too long"},
	}
	for name, want := range cases {
		backup := llmtest.New(llmtest.Turn{Text: "ok"})
		called := false
		p := newFallback(t, llm.FallbackConfig{
			Entries:    []llm.FallbackEntry{{Provider: llmtest.New(llmtest.Turn{Err: want})}, {Provider: backup}},
			OnFallback: func(context.Context, llm.FallbackEvent) { called = true },
		})
		_, err := p.Complete(context.Background(), llm.Request{})
		if err != want {
			t.Errorf("%s: err = %v, want it returned unchanged", name, err)
		}
		if backup.Calls() != 0 || called {
			t.Errorf("%s: switched to the backup", name)
		}
	}
	// A cancelled context reaches the caller as context.Canceled.
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: llmtest.New(llmtest.Turn{Err: fmt.Errorf("send: %w", context.Canceled)})},
		{Provider: llmtest.New(llmtest.Turn{Text: "ok"})},
	}})
	if _, err := p.Complete(context.Background(), llm.Request{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	// A 400 keeps its *APIError.
	p = newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: llmtest.New(llmtest.Turn{Err: &llm.APIError{Status: 400}})},
		{Provider: llmtest.New(llmtest.Turn{Text: "ok"})},
	}})
	var apiErr *llm.APIError
	if _, err := p.Complete(context.Background(), llm.Request{}); !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Errorf("err = %v, want status 400", err)
	}
}

func TestFallbackCustomShouldFallback(t *testing.T) {
	p := newFallback(t, llm.FallbackConfig{
		Entries: []llm.FallbackEntry{
			{Provider: llmtest.New(llmtest.Turn{Err: &llm.APIError{Status: 401}})},
			{Provider: llmtest.New(llmtest.Turn{Text: "ok"})},
		},
		ShouldFallback: func(err error) bool {
			var apiErr *llm.APIError
			return errors.As(err, &apiErr) && apiErr.Status == 401
		},
	})
	resp, err := p.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "ok" {
		t.Errorf("text = %q", resp.Message.Text())
	}
}

func TestFallbackStreamSwitchesOnFirstError(t *testing.T) {
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: llmtest.New(overloaded())},
		{Provider: llmtest.New(llmtest.Turn{Text: "ok"})},
	}})
	resp, err := llm.Collect(context.Background(), p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "ok" {
		t.Errorf("text = %q", resp.Message.Text())
	}
}

// stub is a Provider that answers every call the same way, for any number of
// concurrent calls.
type stub struct {
	err   error
	text  string
	mu    sync.Mutex
	calls int
}

func (s *stub) Complete(context.Context, llm.Request) (*llm.Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: s.text}}}}, nil
}

func (s *stub) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		_, err := s.Complete(ctx, req)
		yield(llm.StreamEvent{}, err)
	}
}

func TestFallbackConcurrent(t *testing.T) {
	primary := &stub{err: &llm.APIError{Status: 529}}
	backup := &stub{text: "ok"}
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: primary}, {Provider: backup}}})

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			resp, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
			if err == nil && resp.Message.Text() != "ok" {
				err = fmt.Errorf("text = %q", resp.Message.Text())
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if backup.calls != 20 {
		t.Errorf("backup calls = %d, want 20", backup.calls)
	}
	// Once the burst is over the primary is cooling down and is skipped.
	before := primary.calls
	if _, err := p.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatal(err)
	}
	if primary.calls != before {
		t.Errorf("primary called while cooling down")
	}
}

func TestFallbackSkipsCoolingWhenOthersFail(t *testing.T) {
	primary := llmtest.New(overloaded())
	b := llmtest.New(llmtest.Turn{Text: "ok"}, overloaded())
	c := llmtest.New(llmtest.Turn{Err: &llm.APIError{Status: 503}})
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: primary}, {Provider: b}, {Provider: c}}})

	if _, err := p.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatal(err)
	}
	_, err := p.Complete(context.Background(), llm.Request{})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 503 {
		t.Errorf("err = %v, want the last entry's 503", err)
	}
	if primary.Calls() != 1 {
		t.Errorf("primary calls = %d, want 1 (still cooling down)", primary.Calls())
	}
}
