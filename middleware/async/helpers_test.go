package async

import (
	"context"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// obj is a tool input.
type obj = map[string]any

// gate is a provider that holds every request until open is closed, then
// answers from inner. A request whose context ends first fails with the
// context's error, so a stopped task never hangs (G19).
type gate struct {
	open      chan struct{}
	inner     *llmtest.Scripted
	entered   chan struct{} // receives one value per request that arrived
	cancelled atomic.Bool
	once      sync.Once
}

func newGate(turns ...llmtest.Turn) *gate {
	return &gate{open: make(chan struct{}), inner: llmtest.New(turns...), entered: make(chan struct{}, 64)}
}

func (g *gate) release() { g.once.Do(func() { close(g.open) }) }

// Stream implements llm.Provider.
func (g *gate) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-g.open:
		case <-ctx.Done():
			g.cancelled.Store(true)
			yield(llm.StreamEvent{}, ctx.Err())
			return
		}
		for ev, err := range g.inner.Stream(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// Complete implements llm.Provider.
func (g *gate) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, g, req)
}

// waitEntered waits until n requests reached g.
func (g *gate) waitEntered(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-g.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the child never called its model")
		}
	}
}

// researcher is a subagent whose model is p.
func researcher(p llm.Provider) subagents.Subagent {
	return subagents.Subagent{Name: "researcher", Description: "Researches.", SystemPrompt: "you research", Provider: p}
}

// harness is a parent agent with the async middleware on thread "t".
type harness struct {
	m  *Middleware
	a  *agent.Agent
	p  *llmtest.Scripted
	st *state.State
	// thread is the run's thread id; "t" unless a test clears it.
	thread string
}

func newHarness(t *testing.T, cfg Config, turns ...llmtest.Turn) *harness {
	t.Helper()
	return newHarnessWith(t, cfg, nil, turns...)
}

// newHarnessWith is newHarness with before ahead of the async middleware in
// the parent's (and so every child's) middleware.
func newHarnessWith(t *testing.T, cfg Config, before []agent.Middleware, turns ...llmtest.Turn) *harness {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	p := llmtest.New(turns...)
	mws := append(slices.Clone(before), m)
	a, err := agent.New(agent.Config{Provider: p, Middleware: mws, PermissionMode: permission.ModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{m: m, a: a, p: p, st: state.New(), thread: "t"}
}

// run runs the parent on text; onEvent, when set, sees every event as it
// arrives.
func (h *harness) run(t *testing.T, ctx context.Context, text string, onEvent func(agent.Event)) ([]agent.Event, *agent.Final) {
	t.Helper()
	var evs []agent.Event
	var final *agent.Final
	in := agent.Input{ThreadID: h.thread, State: h.st, Backend: testutil.Memory(t), Messages: []llm.Message{llm.UserText(text)}}
	for ev := range h.a.Run(ctx, in) {
		evs = append(evs, ev)
		if onEvent != nil {
			onEvent(ev)
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Fatalf("parent run: %+v", final)
	}
	h.st = final.State
	return evs, final
}

// results returns the results of the calls to name, in order.
func results(evs []agent.Event, name string) []tool.Result {
	var out []tool.Result
	for _, ev := range evs {
		if ev.Kind == agent.KindToolResult && ev.ToolName == name && ev.ToolResult != nil {
			out = append(out, *ev.ToolResult)
		}
	}
	return out
}

// result returns the only result of the calls to name.
func result(t *testing.T, evs []agent.Event, name string) tool.Result {
	t.Helper()
	rs := results(evs, name)
	if len(rs) != 1 {
		t.Fatalf("%d results for %s", len(rs), name)
	}
	return rs[0]
}

// call is a scripted turn calling name with in.
func call(id, name string, in obj) llmtest.Turn {
	return llmtest.Turn{ToolCalls: []llmtest.Call{{ID: id, Name: name, Input: in}}}
}

func start(id, typ, desc string) llmtest.Turn {
	return call(id, ToolStart, obj{"subagent_type": typ, "description": desc})
}

// notifications returns the async notifications in the last request; the
// provider never sees Meta, so they are found by their frame.
func notifications(p *llmtest.Scripted) []llm.Message {
	reqs := p.Requests
	var out []llm.Message
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role == llm.RoleUser && strings.HasPrefix(m.Text(), "<task-notification>") {
			out = append(out, m)
		}
	}
	return out
}

// storedNotifications returns the notification messages kept in s.
func storedNotifications(s *state.State) []llm.Message {
	var out []llm.Message
	for _, m := range s.Messages() {
		if m.Meta[NotificationMetaKey] != "" {
			out = append(out, m)
		}
	}
	return out
}

// waitEnded waits until the task id of thread "t", already started, ended.
// A task the Manager no longer knows has ended: its thread was released once
// every task there ended and was announced.
func waitEnded(t *testing.T, m *Middleware, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if j := m.jobs.Get("t", id); j == nil || j.Ended() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not end", id)
		}
		time.Sleep(time.Millisecond)
	}
}

// record returns the stored record id.
func record(t *testing.T, s *state.State, id string) TaskRecord {
	t.Helper()
	records, _ := TasksKey.Get(s)
	for _, r := range records {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no record %s in %+v", id, records)
	return TaskRecord{}
}

// outcomeOf returns how task id of thread "t" ended. It reads the live job
// when the Manager still holds it and otherwise the task's record in s, the
// state of the last run: the job is released once its thread has nothing
// running and every ended task was announced, and that announcement is
// recorded, so a released task always has a final record.
func outcomeOf(t *testing.T, m *Middleware, s *state.State, id string) Outcome {
	t.Helper()
	if j := m.jobs.Get("t", id); j != nil {
		if out, _, ok := j.Handle.outcome(); ok {
			return out
		}
	}
	r := record(t, s, id)
	if !r.finished() {
		t.Fatalf("task %s has no live job and its record is not final: %+v", id, r)
	}
	return Outcome{Status: r.Status, Text: r.Result, Error: r.Error}
}

// fakeRunner records its requests and returns handles that ended with out.
type fakeRunner struct {
	mu   sync.Mutex
	reqs []Request
	out  Outcome
}

func (f *fakeRunner) Start(_ context.Context, req Request) (Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	done := make(chan struct{})
	close(done)
	return &fakeHandle{done: done, out: f.out}, nil
}

type fakeHandle struct {
	done chan struct{}
	out  Outcome
}

func (h *fakeHandle) Done() <-chan struct{}      { return h.done }
func (h *fakeHandle) Outcome() (Outcome, bool)   { return h.out, true }
func (h *fakeHandle) Progress() string           { return "" }
func (h *fakeHandle) Send(string) error          { return nil }
func (h *fakeHandle) Stop(context.Context) error { return nil }

// requestText joins the text of every message of req, tool results
// included.
func requestText(req llm.Request) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		for _, b := range m.Content {
			sb.WriteString(b.Text)
			for _, c := range b.Content {
				sb.WriteString(c.Text)
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// hooks is a test middleware running the functions that are set.
type hooks struct {
	beforeModel func(ctx context.Context, s *state.State)
	afterAgent  func(ctx context.Context, s *state.State)
}

func (h *hooks) Name() string { return "test-hooks" }

func (h *hooks) BeforeModel(ctx context.Context, s *state.State) error {
	if h.beforeModel != nil {
		h.beforeModel(ctx, s)
	}
	return nil
}

func (h *hooks) AfterAgent(ctx context.Context, s *state.State) error {
	if h.afterAgent != nil {
		h.afterAgent(ctx, s)
	}
	return nil
}

// funcRunner is a Runner made of a function.
type funcRunner func(ctx context.Context, req Request) (Handle, error)

func (f funcRunner) Start(ctx context.Context, req Request) (Handle, error) { return f(ctx, req) }

// ctlHandle is a Handle the test ends by hand; Stop ends it with onStop.
type ctlHandle struct {
	done   chan struct{}
	once   sync.Once
	onStop Outcome
	stops  atomic.Int32

	mu       sync.Mutex
	out      Outcome
	progress string
}

func newCtlHandle(onStop Outcome) *ctlHandle {
	return &ctlHandle{done: make(chan struct{}), onStop: onStop}
}

func (h *ctlHandle) end(out Outcome) {
	h.once.Do(func() {
		h.mu.Lock()
		h.out = out
		h.mu.Unlock()
		close(h.done)
	})
}

func (h *ctlHandle) Done() <-chan struct{} { return h.done }

func (h *ctlHandle) Outcome() (Outcome, bool) {
	select {
	case <-h.done:
	default:
		return Outcome{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.out, true
}

func (h *ctlHandle) Progress() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.progress
}

func (h *ctlHandle) Send(string) error { return nil }

func (h *ctlHandle) Stop(context.Context) error {
	h.stops.Add(1)
	h.end(h.onStop)
	return nil
}

// ended returns a handle that already ended with out.
func ended(out Outcome) *ctlHandle {
	h := newCtlHandle(out)
	h.end(out)
	return h
}

// hasRecord reports whether s holds a record id.
func hasRecord(s *state.State, id string) bool {
	records, _ := TasksKey.Get(s)
	return slices.ContainsFunc(records, func(r TaskRecord) bool { return r.ID == id })
}
