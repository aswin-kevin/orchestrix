package coordinator

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/tool"
)

// obj is a tool input.
type obj = map[string]any

func worker(name string, p llm.Provider) subagents.Subagent {
	return subagents.Subagent{Name: name, Description: "The " + name + ".", SystemPrompt: "You are the " + name + ".", Provider: p}
}

// workers are a researcher and a writer answering with their own scripts.
func workers() []subagents.Subagent {
	return []subagents.Subagent{
		worker("researcher", llmtest.New(llmtest.Turn{Text: "research notes"})),
		worker("writer", llmtest.New(llmtest.Turn{Text: "draft text"})),
	}
}

func newCoordinator(t *testing.T, model llm.Provider, cfg Config, opts ...orchestrix.Option) *Coordinator {
	t.Helper()
	c, err := NewWithProvider(model, cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// run runs c on text in thread "t" and returns its events and Final.
func run(t *testing.T, c *Coordinator, text string) ([]agent.Event, *agent.Final) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	var evs []agent.Event
	var final *agent.Final
	in := orchestrix.Text(text)
	in.ThreadID = "t"
	for ev := range c.Run(ctx, in) {
		evs = append(evs, ev)
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Fatalf("coordinator run: %+v", final)
	}
	return evs, final
}

// results returns the text of the results of the coordinator's own calls to
// name, in order.
func results(evs []agent.Event, name string) []string {
	var out []string
	for _, ev := range evs {
		if ev.Kind == agent.KindToolResult && ev.ParentToolID == "" && ev.ToolName == name && ev.ToolResult != nil {
			out = append(out, ev.ToolResult.Flatten())
		}
	}
	return out
}

func TestCoordinatorDelegates(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "s1", Name: async.ToolStart, Input: obj{"subagent_type": "researcher", "description": "Research the topic."}},
			{ID: "s2", Name: async.ToolStart, Input: obj{"subagent_type": "writer", "description": "Write the draft."}},
		}},
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "c1", Name: async.ToolCheck, Input: obj{"task_id": "async_1", "wait": true}},
			{ID: "c2", Name: async.ToolCheck, Input: obj{"task_id": "async_2", "wait": true}},
		}},
		llmtest.Turn{Text: "combined: research notes + draft text"},
	)
	c := newCoordinator(t, p, Config{Workers: workers()})
	evs, final := run(t, c, "Write a report.")

	checks := strings.Join(results(evs, async.ToolCheck), "\n")
	for _, want := range []string{"research notes", "draft text"} {
		if !strings.Contains(checks, want) {
			t.Errorf("check_task results %q lack the worker's answer %q", checks, want)
		}
		if last := p.Requests[len(p.Requests)-1]; !requestHas(last, want) {
			t.Errorf("the coordinator's last request does not show it %q", want)
		}
	}
	if got := p.Requests[0].System; len(got) == 0 || got[0] != DefaultPrompt {
		t.Errorf("system prompt does not start with DefaultPrompt: %q", got)
	}

	records := async.Tasks(final.State)
	if len(records) != 2 {
		t.Fatalf("records = %+v, want two tasks", records)
	}
	for _, r := range records {
		if r.Status != async.StatusCompleted {
			t.Errorf("task %s status = %s, want completed", r.ID, r.Status)
		}
	}
	closeBounded(t, c)
	closeBounded(t, c) // idempotent
}

// remoteRunner records the requests it gets and ends each task at once.
type remoteRunner struct {
	mu   sync.Mutex
	reqs []async.Request
}

func (r *remoteRunner) Start(_ context.Context, req async.Request) (async.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return &doneHandle{done: closedChan()}, nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// doneHandle is a task that completed remotely.
type doneHandle struct{ done chan struct{} }

func (h *doneHandle) Done() <-chan struct{} { return h.done }
func (h *doneHandle) Outcome() (async.Outcome, bool) {
	return async.Outcome{Status: async.StatusCompleted, Text: "remote notes"}, true
}
func (h *doneHandle) Progress() string                   { return "" }
func (h *doneHandle) Send(context.Context, string) error { return async.ErrFinished }
func (h *doneHandle) Stop(context.Context) error         { return nil }

func TestConfigRunner(t *testing.T) {
	runner := &remoteRunner{}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s1", Name: async.ToolStart, Input: obj{"subagent_type": "writer", "description": "Write the draft."}}}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: async.ToolCheck, Input: obj{"task_id": "async_1", "wait": true}}}},
		llmtest.Turn{Text: "done"},
	)
	c := newCoordinator(t, p, Config{Workers: workers(), Runner: runner})
	evs, _ := run(t, c, "Write a report.")
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.reqs) != 1 || runner.reqs[0].SubagentType != "writer" || runner.reqs[0].Description != "Write the draft." {
		t.Fatalf("runner requests = %+v", runner.reqs)
	}
	if checks := strings.Join(results(evs, async.ToolCheck), "\n"); !strings.Contains(checks, "remote notes") {
		t.Fatalf("check_task results = %q, want the runner's answer", checks)
	}
}

// requestHas reports whether any message of req contains text.
func requestHas(req llm.Request, text string) bool {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, text) || strings.Contains(llm.Message{Content: b.Content}.Text(), text) {
				return true
			}
		}
	}
	return false
}

func closeBounded(t *testing.T, c *Coordinator) {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = c.Close(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(async.DefaultStopWait + testTimeout):
		t.Fatal("Close did not return")
	}
}

func TestCoordinatorCloseStopsRunningWorkers(t *testing.T) {
	slow := newEcho()
	slow.block = true
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "s1", Name: async.ToolStart, Input: obj{"subagent_type": "researcher", "description": "Research."}},
		}},
		llmtest.Turn{Text: "started"},
		llmtest.Turn{Text: "still started"}, // after the stop reminder
	)
	c := newCoordinator(t, p, Config{Workers: []subagents.Subagent{worker("researcher", slow)}})
	run(t, c, "Go.")
	select {
	case <-slow.entered:
	case <-time.After(testTimeout):
		t.Fatal("the worker never called its model")
	}
	closeBounded(t, c)
	if running, _ := slow.stats(); running != 0 {
		t.Errorf("%d worker model calls still running after Close", running)
	}
}

func TestCoordinatorMaxConcurrency(t *testing.T) {
	slow := newEcho()
	slow.block = true
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "s1", Name: async.ToolStart, Input: obj{"subagent_type": "researcher", "description": "One."}},
			{ID: "s2", Name: async.ToolStart, Input: obj{"subagent_type": "researcher", "description": "Two."}},
		}},
		llmtest.Turn{Text: "done"},
		llmtest.Turn{Text: "done"}, // after the stop reminder
	)
	c := newCoordinator(t, p, Config{Workers: []subagents.Subagent{worker("researcher", slow)}, MaxConcurrency: 1})
	evs, _ := run(t, c, "Go.")
	refused := 0
	for _, r := range results(evs, async.ToolStart) {
		if strings.Contains(r, "too many background tasks") {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("start_task results %q: want exactly one refused at MaxConcurrency 1", results(evs, async.ToolStart))
	}
}

func TestCoordinatorOffersOnlyWorkers(t *testing.T) {
	c := newCoordinator(t, llmtest.New(), Config{Workers: workers()})
	want := []any{"researcher", "writer"}
	for _, name := range []string{subagents.ToolTask, async.ToolStart} {
		tl, ok := c.Tool(name)
		if !ok {
			t.Errorf("no %s tool", name)
			continue
		}
		props, _ := tl.Schema()["properties"].(map[string]any)
		typ, _ := props["subagent_type"].(map[string]any)
		if got, _ := typ["enum"].([]any); !slices.Equal(got, want) {
			t.Errorf("%s subagent_type enum = %v, want %v", name, typ["enum"], want)
		}
	}
	var names []string
	for _, m := range c.Config().Middleware {
		names = append(names, m.Name())
	}
	if i := slices.Index(names, "subagents"); i < 0 || i+1 >= len(names) || names[i+1] != "async" {
		t.Errorf("stack = %v, want async right after subagents", names)
	}
}

func TestCoordinatorRequiresWorkers(t *testing.T) {
	if _, err := NewWithProvider(llmtest.New(), Config{}); err == nil {
		t.Error("New without workers succeeded")
	}
	if _, err := NewWithProvider(llmtest.New(), Config{Workers: workers(), MaxConcurrency: -1}); err == nil {
		t.Error("New with a negative MaxConcurrency succeeded")
	}
	bad := worker("researcher", llmtest.New())
	bad.Description = ""
	if _, err := NewWithProvider(llmtest.New(), Config{Workers: []subagents.Subagent{bad}}); err == nil {
		t.Error("New with an invalid worker succeeded")
	}
	if _, err := New("nocolon", Config{Workers: workers()}); err == nil {
		t.Error("New with an invalid spec succeeded")
	}
}

func TestCoordinatorOptionsApply(t *testing.T) {
	c := newCoordinator(t, llmtest.New(), Config{Workers: workers(), Prompt: "Custom coordinator."}, orchestrix.WithMaxTurns(1))
	cfg := c.Config()
	if cfg.MaxTurns != 1 {
		t.Errorf("MaxTurns = %d, want 1", cfg.MaxTurns)
	}
	if !slices.Equal(cfg.System, []string{"Custom coordinator."}) {
		t.Errorf("System = %q, want only the configured prompt", cfg.System)
	}

	later := newCoordinator(t, llmtest.New(), Config{Workers: workers()}, orchestrix.WithSystemPrompt("Host prompt."))
	if got := later.Config().System; !slices.Equal(got, []string{"Host prompt."}) {
		t.Errorf("System = %q, want the later option to win", got)
	}
}

func TestCoordinatorRejectsReplacedAsync(t *testing.T) {
	other, err := async.New(async.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close(context.Background()) }()
	for name, opt := range map[string]orchestrix.Option{
		"WithAsync":         orchestrix.WithAsync(other),
		"WithoutMiddleware": orchestrix.WithoutMiddleware("async"),
		"WithMiddleware":    orchestrix.WithMiddleware(other),
	} {
		c, err := NewWithProvider(llmtest.New(), Config{Workers: workers()}, opt)
		if err == nil || !strings.Contains(err.Error(), "must not replace or remove the async middleware") {
			t.Errorf("%s: New = %v, %v; want the replaced-async error", name, c, err)
		}
	}
}

func TestNewWithProviderNil(t *testing.T) {
	c, err := NewWithProvider(nil, Config{Workers: []subagents.Subagent{{Name: "w", Description: "d", SystemPrompt: "p"}}})
	if err == nil || !strings.Contains(err.Error(), "provider must not be nil") {
		t.Fatalf("err = %v, want nil-provider error", err)
	}
	if c != nil {
		t.Error("coordinator returned alongside the error")
	}
}

// settled is one OnSettled call.
type settled struct {
	thread string
	rec    async.TaskRecord
}

func TestCoordinatorResolvesWaitingWorker(t *testing.T) {
	var runs atomic.Int32
	publish := tool.New("publish", "Publishes.", func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
		runs.Add(1)
		return tool.Text("written"), nil
	})
	w := worker("writer", llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w1", Name: "publish", Input: obj{}}}},
		llmtest.Turn{Text: "wrote it"},
	))
	w.Tools = []tool.Tool{publish}
	reports := make(chan settled, 4)
	var events atomic.Int32
	var mu sync.Mutex
	eventThreads := map[string]bool{}
	cfg := Config{
		Workers: []subagents.Subagent{w},
		OnSettled: func(_ context.Context, threadID string, rec async.TaskRecord) {
			reports <- settled{threadID, rec}
		},
		OnEvent: func(threadID, taskID string, _ agent.Event) {
			events.Add(1)
			mu.Lock()
			eventThreads[threadID+" "+taskID] = true
			mu.Unlock()
		},
	}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s1", Name: async.ToolStart, Input: obj{"subagent_type": "writer", "description": "Write."}}}},
		llmtest.Turn{Text: "started"},
		llmtest.Turn{Text: "still started"}, // after the stop reminder
	)
	c := newCoordinator(t, p, cfg)
	run(t, c, "Go.")

	next := func() settled {
		t.Helper()
		select {
		case s := <-reports:
			return s
		case <-time.After(testTimeout):
			t.Fatal("no OnSettled report")
			return settled{}
		}
	}
	first := next()
	if first.thread != "t" || first.rec.Status != async.StatusWaiting || first.rec.Pending == nil {
		t.Fatalf("first report = %+v, want async_1 waiting on t", first)
	}
	var decisions []agent.Decision
	for _, req := range first.rec.Pending.Requests {
		decisions = append(decisions, agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Approve})
	}
	if err := c.Resolve(t.Context(), "t", first.rec.ID, decisions); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if second := next(); second.rec.Status != async.StatusCompleted || second.rec.Result != "wrote it" {
		t.Fatalf("second report = %+v, want the worker completed", second)
	}
	if runs.Load() != 1 {
		t.Fatalf("publish ran %d times, want once", runs.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if events.Load() == 0 || len(eventThreads) != 1 || !eventThreads["t async_1"] {
		t.Fatalf("OnEvent saw %d events from %v, want async_1 on t", events.Load(), eventThreads)
	}
}
