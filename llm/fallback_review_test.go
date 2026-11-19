package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// maxTokensServer answers every request with a 400 after recording the
// request's max_tokens.
func maxTokensServer(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	got := -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		got = req.MaxTokens
		mu.Unlock()
		http.Error(w, `{"error":{"message":"stop here"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func TestFallbackFillsModelFromNewProvider(t *testing.T) {
	srv, maxTokens := maxTokensServer(t)
	backup, err := llm.New(llm.Config{
		Format: llm.FormatOpenAI, Model: "gpt-4o", APIKey: "sk-fake",
		BaseURL: srv.URL, MaxRetries: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: llmtest.New(overloaded())},
		{Provider: backup},
	}})
	entries := llm.FallbackEntries(p)
	if entries[1].Model != "gpt-4o" || entries[1].Name != "gpt-4o" {
		t.Errorf("backup entry = Model %q Name %q, want gpt-4o for both", entries[1].Model, entries[1].Name)
	}
	// The primary's request model may differ from the one it was built
	// with, so the primary is never filled.
	if entries[0].Model != "" {
		t.Errorf("primary Model = %q, want empty", entries[0].Model)
	}
	_, _ = p.Complete(context.Background(), llm.Request{MaxTokens: 64_000})
	if got := maxTokens(); got != 16_384 {
		t.Errorf("backup max_tokens = %d, want 16384 (gpt-4o profile)", got)
	}
}

func TestFallbackExplicitModelWins(t *testing.T) {
	llm.RegisterProfile("fallback-test-explicit", llm.Profile{ContextWindow: 100_000, MaxOutput: 500})
	srv, maxTokens := maxTokensServer(t)
	backup, err := llm.New(llm.Config{
		Format: llm.FormatOpenAI, Model: "gpt-4o", APIKey: "sk-fake",
		BaseURL: srv.URL, MaxRetries: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: llmtest.New(overloaded())},
		{Provider: backup, Model: "fallback-test-explicit"},
	}})
	if got := llm.FallbackEntries(p)[1].Model; got != "fallback-test-explicit" {
		t.Errorf("Model = %q, want the explicit one", got)
	}
	_, _ = p.Complete(context.Background(), llm.Request{MaxTokens: 64_000})
	if got := maxTokens(); got != 500 {
		t.Errorf("backup max_tokens = %d, want 500", got)
	}
}

func TestFallbackDefaultNameCollision(t *testing.T) {
	ok := llmtest.New()
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: ok, Name: "primary"},
		{Provider: ok, Model: "gpt-4o"},
		{Provider: ok, Model: "gpt-4o"},
	}})
	var names []string
	for _, e := range llm.FallbackEntries(p) {
		names = append(names, e.Name)
	}
	if want := []string{"primary", "gpt-4o", "gpt-4o-2"}; !equalStrings(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	// A defaulted name never takes an explicit one, wherever it appears.
	p = newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: ok, Model: "m"},
		{Provider: ok, Name: "m"},
	}})
	names = names[:0]
	for _, e := range llm.FallbackEntries(p) {
		names = append(names, e.Name)
	}
	if want := []string{"m-0", "m"}; !equalStrings(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	// Explicit duplicates still fail.
	if _, err := llm.NewFallback(llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: ok, Name: "x"}, {Provider: ok, Name: "x"},
	}}); err == nil {
		t.Error("explicit duplicate names: NewFallback succeeded, want an error")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// In-stream error bodies as the adapters record them (APIError.Body is the
// event's data).
const (
	anthropicOverloadedEvent = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	responsesFailedEvent     = `{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"server_error","message":"boom"}}}`
	responsesErrorEvent      = `{"type":"error","code":"rate_limit_exceeded","message":"slow down","param":null}`
	chatChunkError           = `{"error":{"message":"busy","type":"server_error","code":null}}`
)

func TestDefaultShouldFallbackInStream(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"anthropic overloaded": {&llm.APIError{Body: anthropicOverloadedEvent}, true},
		"anthropic rate limit": {&llm.APIError{Body: `{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`}, true},
		"anthropic api error":  {&llm.APIError{Body: `{"type":"error","error":{"type":"api_error","message":"x"}}`}, true},
		"responses failed":     {&llm.APIError{Body: responsesFailedEvent}, true},
		"responses error":      {&llm.APIError{Body: responsesErrorEvent}, true},
		"chat chunk error":     {&llm.APIError{Body: chatChunkError}, true},
		"wrapped":              {errors.Join(errors.New("stream"), &llm.APIError{Body: anthropicOverloadedEvent}), true},
		"invalid request":      {&llm.APIError{Body: `{"type":"error","error":{"type":"invalid_request_error","message":"x"}}`}, false},
		"overflow in stream":   {&llm.APIError{Body: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`}, false},
		"overflow named api":   {&llm.APIError{Body: `{"type":"error","error":{"type":"api_error","message":"prompt is too long"}}`}, false},
		"message mentions it":  {&llm.APIError{Body: `{"type":"error","error":{"type":"invalid_request_error","message":"not a server_error"}}`}, false},
		"not json":             {&llm.APIError{Body: "overloaded_error"}, false},
		"status 400 with type": {&llm.APIError{Status: 400, Body: anthropicOverloadedEvent}, false},
		"canceled":             {errors.Join(context.Canceled, &llm.APIError{Body: anthropicOverloadedEvent}), false},
	}
	for name, c := range cases {
		if got := llm.DefaultShouldFallback(c.err); got != c.want {
			t.Errorf("%s: DefaultShouldFallback = %v, want %v", name, got, c.want)
		}
	}
}

func TestFallbackStreamSwitchesOnInStreamOverload(t *testing.T) {
	primary := &eventStub{
		events: []llm.StreamEvent{{Type: llm.EvMessageStart}},
		err:    &llm.APIError{Body: anthropicOverloadedEvent},
	}
	backup := llmtest.New(llmtest.Turn{Text: "ok"})
	var events []llm.FallbackEvent
	p := newFallback(t, llm.FallbackConfig{
		Entries:    []llm.FallbackEntry{{Provider: primary, Name: "a"}, {Provider: backup, Name: "b"}},
		OnFallback: func(_ context.Context, ev llm.FallbackEvent) { events = append(events, ev) },
	})
	resp, err := llm.Collect(context.Background(), p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "ok" || len(events) != 1 {
		t.Errorf("text %q, events %+v; want ok and one switch", resp.Message.Text(), events)
	}
}

// cancelling is a Provider that cancels the call's context and then fails
// with a 529, as a request racing a cancellation can.
type cancelling struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	calls  int
}

func (c *cancelling) fail() error {
	c.mu.Lock()
	c.calls++
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return &llm.APIError{Status: 529}
}

func (c *cancelling) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, c.fail()
}

func (c *cancelling) Stream(context.Context, llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) { yield(llm.StreamEvent{}, c.fail()) }
}

func TestFallbackNoSwitchWhenContextDone(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			primary := &cancelling{cancel: cancel}
			backup := llmtest.New(llmtest.Turn{Text: "ok"}, llmtest.Turn{Text: "ok"})
			called := false
			p := newFallback(t, llm.FallbackConfig{
				Entries:    []llm.FallbackEntry{{Provider: primary}, {Provider: backup}},
				OnFallback: func(context.Context, llm.FallbackEvent) { called = true },
			})
			var err error
			if mode == "complete" {
				_, err = p.Complete(ctx, llm.Request{})
			} else {
				_, err = llm.Collect(ctx, p, llm.Request{})
			}
			var apiErr *llm.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 529 {
				t.Errorf("err = %v, want the primary's 529", err)
			}
			if called || backup.Calls() != 0 {
				t.Errorf("switched after the context was cancelled")
			}
			// No cool-down: the next call reaches the primary again.
			primary.mu.Lock()
			primary.cancel = nil
			primary.mu.Unlock()
			_, _ = p.Complete(context.Background(), llm.Request{})
			primary.mu.Lock()
			calls := primary.calls
			primary.mu.Unlock()
			if calls != 2 {
				t.Errorf("primary calls = %d, want 2 (no cool-down)", calls)
			}
		})
	}
}
