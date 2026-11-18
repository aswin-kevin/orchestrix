package agent

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

type echoInput struct {
	Text string `json:"text"`
}

// echoTool returns its input text; it is read-only and concurrency-safe.
func echoTool() tool.Tool {
	return tool.New("echo", "Echo the text back.",
		func(_ context.Context, in echoInput, _ *tool.Call) (tool.Result, error) {
			return tool.Text(in.Text), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())
}

// mustNew builds an agent or fails the test.
func mustNew(t *testing.T, cfg Config) *Agent {
	t.Helper()
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// collect drains a run and returns every event plus the final outcome. It
// fails the test unless KindFinal arrives exactly once, as the last event.
func collect(t *testing.T, seq iter.Seq2[Event, error]) ([]Event, *Final) {
	t.Helper()
	var events []Event
	for ev := range seq {
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatal("run produced no events")
	}
	last := events[len(events)-1]
	if last.Kind != KindFinal || last.Final == nil {
		t.Fatalf("last event = %+v, want KindFinal", last)
	}
	for _, ev := range events[:len(events)-1] {
		if ev.Kind == KindFinal {
			t.Fatal("KindFinal emitted before the end of the run")
		}
	}
	return events, last.Final
}

func userInput(text string) Input {
	return Input{Messages: []llm.Message{llm.UserText(text)}}
}

// lastToolResults returns the tool_result blocks of the last message in s.
func lastToolResults(t *testing.T, s *state.State) []llm.Block {
	t.Helper()
	msgs := s.Messages()
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("last message role = %s, want user", last.Role)
	}
	return last.Content
}

func TestRunTextOnly(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "hello there"})
	a := mustNew(t, Config{Provider: p})

	events, final := collect(t, a.Run(context.Background(), userInput("hi")))

	if final.Reason != ReasonCompleted || final.Text != "hello there" || final.Err != nil {
		t.Fatalf("final = %+v, want completed with text", final)
	}
	if events[0].Kind != KindText || events[0].Text != "hello there" {
		t.Fatalf("first event = %+v, want text delta", events[0])
	}
	for _, ev := range events {
		if ev.Agent != DefaultAgentName {
			t.Fatalf("event %s has Agent %q, want %q", ev.Kind, ev.Agent, DefaultAgentName)
		}
	}
	msgs := final.State.Messages()
	if len(msgs) != 2 || msgs[1].Role != llm.RoleAssistant {
		t.Fatalf("state messages = %+v, want user + assistant", msgs)
	}
}

func TestRunToolThenAnswer(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "t1", Name: "echo", Input: map[string]any{"text": "ping"}}}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{echoTool()}})

	events, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted || final.Turns != 2 || final.Text != "done" {
		t.Fatalf("final = %+v, want completed after 2 turns", final)
	}
	second := p.Requests[1]
	last := second.Messages[len(second.Messages)-1]
	if last.Role != llm.RoleUser || len(last.Content) != 1 {
		t.Fatalf("last request message = %+v, want one tool_result", last)
	}
	res := last.Content[0]
	if res.Type != llm.BlockToolResult || res.ToolUseID != "t1" || res.IsError || res.Content[0].Text != "ping" {
		t.Fatalf("tool result = %+v, want echo output for t1", res)
	}
	assertToolEvents(t, events, "t1")
}

// assertToolEvents checks that KindToolUse precedes KindToolResult for id.
func assertToolEvents(t *testing.T, events []Event, id string) {
	t.Helper()
	useAt, resultAt := -1, -1
	for i, ev := range events {
		switch {
		case ev.Kind == KindToolUse && ev.ToolID == id:
			useAt = i
		case ev.Kind == KindToolResult && ev.ToolID == id:
			resultAt = i
			if ev.ToolResult == nil {
				t.Fatalf("KindToolResult for %s has no ToolResult", id)
			}
		}
	}
	if useAt < 0 || resultAt < useAt {
		t.Fatalf("tool events for %s: use at %d, result at %d", id, useAt, resultAt)
	}
}

func TestRunUsageAndInputState(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "ok", Usage: llm.Usage{Input: 10, Output: 3}})
	a := mustNew(t, Config{Provider: p})
	s := state.New()
	s.AppendMessages(llm.UserText("earlier"))

	events, final := collect(t, a.Run(context.Background(), Input{State: s, Messages: []llm.Message{llm.UserText("now")}}))

	if final.State != s {
		t.Fatal("Final.State is not the state passed in")
	}
	if got := len(p.Requests[0].Messages); got != 2 {
		t.Fatalf("request has %d messages, want 2 (history + input)", got)
	}
	if final.Usage != (llm.Usage{Input: 10, Output: 3}) {
		t.Fatalf("final usage = %+v", final.Usage)
	}
	var sawUsage bool
	for _, ev := range events {
		sawUsage = sawUsage || (ev.Kind == KindUsage && ev.Usage != nil && ev.Usage.Output == 3)
	}
	if !sawUsage {
		t.Fatal("no KindUsage event")
	}
	if p.Requests[0].MaxTokens != llm.DefaultProfile.MaxOutput {
		t.Fatalf("MaxTokens = %d, want profile max output", p.Requests[0].MaxTokens)
	}
}

func TestRunStripsMetaFromRequestView(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a := mustNew(t, Config{Provider: p})
	msg := llm.UserText("hi")
	msg.Meta = map[string]string{"source": "test"}

	_, final := collect(t, a.Run(context.Background(), Input{Messages: []llm.Message{msg}}))

	if p.Requests[0].Messages[0].Meta != nil {
		t.Fatal("request view still carries Meta")
	}
	if final.State.Messages()[0].Meta == nil {
		t.Fatal("Meta was removed from State")
	}
}

func TestParallelReadOnlyBatch(t *testing.T) {
	const sleep = 100 * time.Millisecond
	var (
		mu       sync.Mutex
		inFlight int
		peak     int
		lastEnd  time.Time
		serialAt time.Time
	)
	slow := tool.New("slow", "Sleep, then echo.",
		func(ctx context.Context, in echoInput, _ *tool.Call) (tool.Result, error) {
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return tool.Result{}, ctx.Err()
			}
			mu.Lock()
			inFlight--
			lastEnd = time.Now()
			mu.Unlock()
			return tool.Text(in.Text), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())
	serial := tool.New("serial", "Record when it ran.",
		func(_ context.Context, in echoInput, _ *tool.Call) (tool.Result, error) {
			mu.Lock()
			serialAt = time.Now()
			mu.Unlock()
			return tool.Text(in.Text), nil
		},
		tool.WithReadOnly())
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "a", Name: "slow", Input: map[string]any{"text": "A"}},
			{ID: "b", Name: "slow", Input: map[string]any{"text": "B"}},
			{ID: "c", Name: "slow", Input: map[string]any{"text": "C"}},
			{ID: "d", Name: "serial", Input: map[string]any{"text": "D"}},
		}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{slow, serial}})

	start := time.Now()
	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if elapsed := lastEnd.Sub(start); elapsed >= 250*time.Millisecond {
		t.Fatalf("batch of 3 x %v took %v, want < 250ms", sleep, elapsed)
	}
	if peak != 3 {
		t.Fatalf("peak concurrency = %d, want 3", peak)
	}
	if serialAt.Before(lastEnd) {
		t.Fatal("the non-concurrent call ran before the batch finished")
	}
	results := final.State.Messages()[2].Content
	for i, want := range []string{"A", "B", "C", "D"} {
		if results[i].ToolUseID != strings.ToLower(want) || results[i].Content[0].Text != want {
			t.Fatalf("result %d = %+v, want %s", i, results[i], want)
		}
	}
}

func TestMaxConcurrencyBoundsBatch(t *testing.T) {
	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	slow := tool.New("slow", "Sleep briefly.",
		func(_ context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return tool.Text("ok"), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())
	calls := make([]llmtest.Call, 4)
	for i := range calls {
		calls[i] = llmtest.Call{ID: string(rune('a' + i)), Name: "slow"}
	}
	p := llmtest.New(llmtest.Turn{ToolCalls: calls}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{slow}, MaxConcurrency: 2})

	collect(t, a.Run(context.Background(), userInput("go")))

	if peak != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak)
	}
}

func TestCancelDuringToolsPairsResults(t *testing.T) {
	baseline := runtime.NumGoroutine()
	started := make(chan struct{}, 2)
	var serialRan bool
	blocking := tool.New("block", "Block until cancelled.",
		func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
			started <- struct{}{}
			<-ctx.Done()
			return tool.Result{}, ctx.Err()
		},
		tool.WithReadOnly(), tool.WithConcurrent())
	serial := tool.New("serial", "Must never run.",
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
			serialRan = true
			return tool.Text("ran"), nil
		},
		tool.WithReadOnly())
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{
		{ID: "b1", Name: "block"}, {ID: "b2", Name: "block"}, {ID: "s1", Name: "serial"},
	}})
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{blocking, serial}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		<-started
		cancel()
	}()
	_, final := collect(t, a.Run(ctx, userInput("go")))

	if final.Reason != ReasonAbortedTools || !errors.Is(final.Err, context.Canceled) {
		t.Fatalf("final = %+v, want aborted_tools with context.Canceled", final)
	}
	if serialRan {
		t.Fatal("a call scheduled after cancellation ran")
	}
	results := lastToolResults(t, final.State)
	for i, id := range []string{"b1", "b2", "s1"} {
		r := results[i]
		if r.ToolUseID != id || !r.IsError || r.Content[0].Text != "Tool call cancelled." {
			t.Fatalf("result %d = %+v, want cancelled result for %s", i, r, id)
		}
	}
	waitForGoroutines(t, baseline)
}

// waitForGoroutines fails unless the goroutine count drops back to baseline
// within a second, which shows the run left nothing running behind it.
func waitForGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d, want <= %d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// blockingProvider streams one text delta, then blocks until its context is
// cancelled, recording the context error it saw.
type blockingProvider struct {
	mu     sync.Mutex
	ctxErr error
}

func (p *blockingProvider) Stream(ctx context.Context, _ llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.EvTextDelta, Text: "partial"}, nil) {
			return
		}
		<-ctx.Done()
		p.mu.Lock()
		p.ctxErr = ctx.Err()
		p.mu.Unlock()
		yield(llm.StreamEvent{}, ctx.Err())
	}
}

func (p *blockingProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, p, req)
}

func TestConsumerBreakStopsRun(t *testing.T) {
	baseline := runtime.NumGoroutine()
	p := &blockingProvider{}
	afterAgent := &hookRecorder{name: "rec", trace: &[]string{}}
	a := mustNew(t, Config{Provider: p, Middleware: []Middleware{afterAgent}})

	for range a.Run(context.Background(), userInput("hi")) {
		break
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if !errors.Is(p.ctxErr, context.Canceled) {
		t.Fatalf("provider context error = %v, want context.Canceled", p.ctxErr)
	}
	for _, entry := range *afterAgent.trace {
		if strings.HasPrefix(entry, "AfterAgent") {
			t.Fatal("AfterAgent ran after the consumer stopped reading")
		}
	}
	waitForGoroutines(t, baseline)
}

func TestCancelDuringStreaming(t *testing.T) {
	p := &blockingProvider{}
	a := mustNew(t, Config{Provider: p})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var final *Final
	for ev := range a.Run(ctx, userInput("hi")) {
		if ev.Kind == KindText {
			cancel()
		}
		if ev.Kind == KindFinal {
			final = ev.Final
		}
	}

	if final == nil || final.Reason != ReasonAbortedStreaming {
		t.Fatalf("final = %+v, want aborted_streaming", final)
	}
	if n := len(final.State.Messages()); n != 1 {
		t.Fatalf("state has %d messages, want only the user message", n)
	}
}

func TestOverflowReason(t *testing.T) {
	overflow := &llm.APIError{Status: 400, Body: "prompt is too long"}
	p := llmtest.New(llmtest.Turn{Err: overflow})
	a := mustNew(t, Config{Provider: p})

	_, final := collect(t, a.Run(context.Background(), userInput("hi")))

	if final.Reason != ReasonPromptTooLong || !errors.Is(final.Err, overflow) {
		t.Fatalf("final = %+v, want prompt_too_long wrapping the API error", final)
	}
}

func TestModelErrorReason(t *testing.T) {
	failure := &llm.APIError{Status: 500, Body: "boom"}
	p := llmtest.New(llmtest.Turn{Err: failure})
	a := mustNew(t, Config{Provider: p})

	var lastErr error
	var final *Final
	for ev, err := range a.Run(context.Background(), userInput("hi")) {
		lastErr = err
		final = ev.Final
	}

	if final == nil || final.Reason != ReasonModelError || !errors.Is(final.Err, failure) {
		t.Fatalf("final = %+v, want model_error", final)
	}
	if !errors.Is(lastErr, failure) {
		t.Fatalf("iterator error with KindFinal = %v, want the model error", lastErr)
	}
}

// retryTwice is a ModelWrapper that calls the model twice, keeping the second
// answer.
type retryTwice struct{}

func (retryTwice) Name() string { return "retry" }

func (retryTwice) WrapModel(ctx context.Context, req *ModelRequest, next ModelHandler) (*llm.Response, error) {
	if _, err := next(ctx, req); err != nil {
		return nil, err
	}
	return next(ctx, req)
}

func TestRetryEventWhenModelCalledTwice(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "first"}, llmtest.Turn{Text: "second"})
	a := mustNew(t, Config{Provider: p, Middleware: []Middleware{retryTwice{}}})

	events, final := collect(t, a.Run(context.Background(), userInput("hi")))

	var kinds []EventKind
	for _, ev := range events {
		if ev.Kind == KindText || ev.Kind == KindRetry {
			kinds = append(kinds, ev.Kind)
		}
	}
	if want := []EventKind{KindText, KindRetry, KindText}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	if final.Text != "second" {
		t.Fatalf("final text = %q, want the retried answer", final.Text)
	}
}

// hookRecorder logs every lifecycle hook it receives into trace.
type hookRecorder struct {
	name  string
	trace *[]string
	fail  string // hook name that returns an error
}

func (h *hookRecorder) Name() string { return h.name }

func (h *hookRecorder) record(hook string) error {
	*h.trace = append(*h.trace, hook+":"+h.name)
	if hook == h.fail {
		return errors.New(hook + " failed")
	}
	return nil
}

func (h *hookRecorder) BeforeAgent(context.Context, *state.State) error {
	return h.record("BeforeAgent")
}

func (h *hookRecorder) BeforeModel(context.Context, *state.State) error {
	return h.record("BeforeModel")
}

func (h *hookRecorder) AfterModel(context.Context, *state.State, *llm.Response) error {
	return h.record("AfterModel")
}

func (h *hookRecorder) AfterAgent(context.Context, *state.State) error {
	return h.record("AfterAgent")
}

func TestHooksOrder(t *testing.T) {
	var trace []string
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "t1", Name: "echo", Input: map[string]any{"text": "x"}}}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{
		Provider:   p,
		Tools:      []tool.Tool{echoTool()},
		Middleware: []Middleware{&hookRecorder{name: "a", trace: &trace}, &hookRecorder{name: "b", trace: &trace}},
	})

	collect(t, a.Run(context.Background(), userInput("go")))

	want := []string{
		"BeforeAgent:a", "BeforeAgent:b",
		"BeforeModel:a", "BeforeModel:b", "AfterModel:b", "AfterModel:a",
		"BeforeModel:a", "BeforeModel:b", "AfterModel:b", "AfterModel:a",
		"AfterAgent:b", "AfterAgent:a",
	}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("hook trace =\n%v\nwant\n%v", trace, want)
	}
}

func TestHookErrorEndsRun(t *testing.T) {
	var trace []string
	p := llmtest.New(llmtest.Turn{Text: "never"})
	a := mustNew(t, Config{
		Provider:   p,
		Middleware: []Middleware{&hookRecorder{name: "a", trace: &trace, fail: "BeforeModel"}},
	})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonMiddlewareError || final.Err == nil {
		t.Fatalf("final = %+v, want middleware_error", final)
	}
	if p.Calls() != 0 {
		t.Fatal("model was called after BeforeModel failed")
	}
	if want := []string{"BeforeAgent:a", "BeforeModel:a", "AfterAgent:a"}; !reflect.DeepEqual(trace, want) {
		t.Fatalf("hook trace = %v, want %v (AfterAgent still runs)", trace, want)
	}
}

// toolOwner is a middleware that contributes a tool.
type toolOwner struct{ tool tool.Tool }

func (toolOwner) Name() string         { return "owner" }
func (o toolOwner) Tools() []tool.Tool { return []tool.Tool{o.tool} }

func TestToolProviderMiddlewareToolsRegistered(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "t1", Name: "echo", Input: map[string]any{"text": "from mw"}}}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{Provider: p, Middleware: []Middleware{toolOwner{tool: echoTool()}}})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if got := p.Requests[0].Tools; len(got) != 1 || got[0].Name != "echo" {
		t.Fatalf("request tools = %+v, want the middleware's echo", got)
	}
	res := final.State.Messages()[2].Content[0]
	if res.IsError || res.Content[0].Text != "from mw" {
		t.Fatalf("tool result = %+v, want echo output", res)
	}
}

func TestNewValidation(t *testing.T) {
	p := llmtest.New()
	cases := map[string]Config{
		"nil provider":             {},
		"duplicate tool":           {Provider: p, Tools: []tool.Tool{echoTool(), echoTool()}},
		"duplicate across sources": {Provider: p, Tools: []tool.Tool{echoTool()}, Middleware: []Middleware{toolOwner{tool: echoTool()}}},
		"nil middleware":           {Provider: p, Middleware: []Middleware{nil}},
		"duplicate middleware":     {Provider: p, Middleware: []Middleware{retryTwice{}, retryTwice{}}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Fatal("New succeeded, want an error")
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New(), Model: "gpt-4o", Settlement: &Settlement{}})

	cfg := a.Config()
	if cfg.Name != DefaultAgentName || cfg.MaxConcurrency != DefaultMaxConcurrency ||
		cfg.MaxToolOutputChars != DefaultMaxToolOutputChars || cfg.Settlement.MaxTurns != DefaultSettlementMaxTurns {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.Profile == nil || *cfg.Profile != llm.LookupProfile("gpt-4o") {
		t.Fatalf("profile = %+v, want the looked-up one", cfg.Profile)
	}
	cfg.Profile.MaxOutput = 1
	cfg.Settlement.MaxTurns = 99
	if again := a.Config(); again.Profile.MaxOutput == 1 || again.Settlement.MaxTurns == 99 {
		t.Fatal("mutating the returned Config changed the agent")
	}
}

func TestRunEventsCarryAgentName(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "t1", Name: "echo", Input: map[string]any{"text": "x"}}}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{Name: "worker", Provider: p, Tools: []tool.Tool{echoTool()}, PermissionMode: permission.ModeDefault})

	events, _ := collect(t, a.Run(context.Background(), userInput("go")))

	for _, ev := range events {
		if ev.Agent != "worker" {
			t.Fatalf("event %s has Agent %q, want worker", ev.Kind, ev.Agent)
		}
	}
}

func TestConsumerPanicStopsRun(t *testing.T) {
	baseline := runtime.NumGoroutine()
	p := &blockingProvider{}
	a := mustNew(t, Config{Provider: p})

	func() {
		defer func() { _ = recover() }()
		for range a.Run(context.Background(), userInput("hi")) {
			panic("consumer failed")
		}
	}()

	p.mu.Lock()
	defer p.mu.Unlock()
	if !errors.Is(p.ctxErr, context.Canceled) {
		t.Fatalf("provider context error = %v, want context.Canceled", p.ctxErr)
	}
	waitForGoroutines(t, baseline)
}
