package async

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/agenttest"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	cpmemory "github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Every test here simulates a restart: a new Middleware and a new Agent
// (process 2) over the store process 1 saved to. Process 1's Middleware is
// closed first, as a process that went away holds nothing.

// proc is one process: a parent agent with its own async middleware.
type proc struct {
	m  *Middleware
	a  *agent.Agent
	p  *llmtest.Scripted
	cp checkpoint.Checkpointer
	// st is the parent's State when cp is nil: the host keeps it.
	st *state.State
	// runBackend, when set, is every run's Input.Backend.
	runBackend backend.Backend
}

// newProc builds a process whose parent saves with cp (nil: unsaved) and
// runs its tools on be.
func newProc(t *testing.T, cfg Config, cp checkpoint.Checkpointer, be backend.Backend, before []agent.Middleware, turns ...llmtest.Turn) *proc {
	t.Helper()
	cfg.DisableStopReminder = true
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	p := llmtest.New(turns...)
	a, err := agent.New(agent.Config{
		Provider: p, Middleware: append(slices.Clone(before), m), PermissionMode: permission.ModeDefault,
		Checkpointer: cp, Backend: be,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &proc{m: m, a: a, p: p, cp: cp, st: state.New()}
}

// run runs the parent on thread "t" with text, from the thread's checkpoint
// (or the State the host keeps, without a store); mutate, when set,
// changes that State first.
func (pr *proc) run(t *testing.T, text string, mutate func(*state.State)) []agent.Event {
	t.Helper()
	ctx := context.Background()
	s, v := pr.st, checkpoint.Version("")
	if pr.cp != nil {
		var err error
		s, v, err = pr.cp.Load(ctx, "t")
		if errors.Is(err, checkpoint.ErrNotFound) {
			s, err = state.New(), nil
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if mutate != nil {
		mutate(s)
	}
	var evs []agent.Event
	var final *agent.Final
	in := agent.Input{ThreadID: "t", State: s, Version: v, Backend: pr.runBackend, Messages: []llm.Message{llm.UserText(text)}}
	for ev := range pr.a.Run(ctx, in) {
		evs = append(evs, ev)
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Fatalf("parent run: %+v", final)
	}
	pr.st = final.State
	return evs
}

// crash ends process 1: its tasks stop, its memory is gone.
func (pr *proc) crash(t *testing.T) {
	t.Helper()
	if err := pr.m.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

// blocker answers its first request from inner and holds every later one
// until its context ends; entered receives one value per request.
type blocker struct {
	inner   *llmtest.Scripted
	n       atomic.Int32
	entered chan struct{}
}

func newBlocker(first llmtest.Turn) *blocker {
	return &blocker{inner: llmtest.New(first), entered: make(chan struct{}, 8)}
}

// Stream implements llm.Provider.
func (b *blocker) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		b.entered <- struct{}{}
		if b.n.Add(1) > 1 {
			<-ctx.Done()
			yield(llm.StreamEvent{}, ctx.Err())
			return
		}
		for ev, err := range b.inner.Stream(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// Complete implements llm.Provider.
func (b *blocker) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, b, req)
}

// worker is a researcher on p with the noop tool.
func worker(p llm.Provider, mode permission.Mode) subagents.Subagent {
	spec := researcher(p)
	spec.Tools = []tool.Tool{noop}
	spec.PermissionMode = mode
	return spec
}

// crashWhileRunning runs process 1 on cp: its parent starts async_1, whose
// child answers one tool turn and is blocked on its second model call when
// the process goes away; the parent's saved record shows it running.
func crashWhileRunning(t *testing.T, cp checkpoint.Checkpointer, be backend.Backend, mode permission.Mode) *proc {
	t.Helper()
	b := newBlocker(call("n1", "noop", obj{}))
	p1 := newProc(t, Config{Subagents: []subagents.Subagent{worker(b, mode)}}, cp, be, nil,
		start("s1", "researcher", "slow research"), llmtest.Turn{Text: "started"})
	p1.run(t, "go", func(s *state.State) {
		if err := agent.SetPermissionMode(s, permission.ModeAcceptEdits); err != nil {
			t.Fatal(err)
		}
	})
	for range 2 {
		select {
		case <-b.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the child never reached its second model call")
		}
	}
	if r := record(t, p1.st, "async_1"); r.Status != StatusRunning {
		t.Fatalf("saved record = %+v, want running", r)
	}
	p1.crash(t)
	return p1
}

func TestRunningRecordReattachedAtRunStart(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	crashWhileRunning(t, cp, be, "")

	spy := &modeSpy{}
	g := newGate(llmtest.Turn{Text: "done"})
	p2 := newProc(t, Config{Subagents: []subagents.Subagent{worker(g, "")}}, cp, be, []agent.Middleware{spy.hooks()},
		llmtest.Turn{Text: "checking"}, llmtest.Turn{Text: "noted"})
	p2.run(t, "status?", nil)
	if notes := notifications(p2.p); len(notes) != 0 {
		t.Fatalf("notifications at the first run = %+v, want none", notes)
	}
	if r := record(t, p2.st, "async_1"); r.Status != StatusRunning {
		t.Fatalf("record = %+v, want running", r)
	}
	g.release()
	waitEnded(t, p2.m, "async_1")
	p2.run(t, "more", nil)
	notes := notifications(p2.p)
	if len(notes) != 1 || !strings.Contains(notes[0].Text(), "async_1 (researcher) completed.") || !strings.Contains(notes[0].Text(), "done") {
		t.Fatalf("notifications = %+v", notes)
	}
	if r := record(t, p2.st, "async_1"); r.Status != StatusCompleted || r.Result != "done" || !r.Notified {
		t.Fatalf("record = %+v", r)
	}
	reqs := g.inner.Requests
	if len(reqs) != 1 {
		t.Fatalf("the reattached child made %d requests, want 1", len(reqs))
	}
	text := requestText(reqs[0])
	for _, want := range []string{"slow research", "ok", restartText} {
		if !strings.Contains(text, want) {
			t.Fatalf("the resumed request lacks %q:\n%s", want, text)
		}
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	// The reattached episode runs nested, though no tool call started it.
	if len(spy.depths) != 1 || spy.depths[0] != 1 {
		t.Fatalf("reattached task depths = %v, want [1]", spy.depths)
	}
}

func TestReattachAppliesStricterParentMode(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	crashWhileRunning(t, cp, be, permission.ModeAcceptEdits)

	spy := &modeSpy{}
	child := llmtest.New(llmtest.Turn{Text: "done"})
	p2 := newProc(t, Config{Subagents: []subagents.Subagent{worker(child, permission.ModeAcceptEdits)}}, cp, be, []agent.Middleware{spy.hooks()},
		llmtest.Turn{Text: "checking"})
	p2.run(t, "status?", func(s *state.State) {
		if err := agent.SetPermissionMode(s, permission.ModePlan); err != nil {
			t.Fatal(err)
		}
	})
	waitEnded(t, p2.m, "async_1")
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.modes) != 1 || spy.modes[0] != permission.ModePlan {
		t.Fatalf("reattached task modes = %v, want [plan]", spy.modes)
	}
}

func TestReattachWithoutCheckpointerIsLost(t *testing.T) {
	be := testutil.Memory(t)
	p1 := crashWhileRunning(t, nil, be, "")

	p2 := newProc(t, Config{Subagents: []subagents.Subagent{worker(llmtest.New(llmtest.Turn{Text: "never"}), "")}}, nil, be, nil,
		llmtest.Turn{Text: "ok"})
	p2.st = p1.st // the host kept the parent's State
	p2.run(t, "next", nil)
	notes := notifications(p2.p)
	want := "<task-notification>Background task async_1 (researcher) " + lostNote + "</task-notification>"
	if len(notes) != 1 || notes[0].Text() != want {
		t.Fatalf("notifications = %+v", notes)
	}
	if r := record(t, p2.st, "async_1"); r.Status != StatusLost || !r.Notified {
		t.Fatalf("record = %+v", r)
	}
}

func TestCustomRunnerWithoutReattacherIsLost(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	runner := funcRunner(func(context.Context, Request) (Handle, error) {
		return newCtlHandle(Outcome{Status: StatusCancelled}), nil
	})
	p1 := newProc(t, Config{Runner: runner}, cp, be, nil, start("s1", "general-purpose", "slow"), llmtest.Turn{Text: "started"})
	p1.run(t, "go", nil)
	p1.crash(t)

	p2 := newProc(t, Config{Runner: runner}, cp, be, nil, llmtest.Turn{Text: "ok"})
	p2.run(t, "next", nil)
	notes := notifications(p2.p)
	if len(notes) != 1 || !strings.Contains(notes[0].Text(), lostNote) {
		t.Fatalf("notifications = %+v, want the lost note", notes)
	}
	if r := record(t, p2.st, "async_1"); r.Status != StatusLost || !r.Notified || r.Ended.IsZero() {
		t.Fatalf("record = %+v, want lost and announced", r)
	}
}

// crashWhileWaiting runs process 1 on cp: its task async_1 waits to write a
// file, and the parent's saved record shows it waiting.
func crashWhileWaiting(t *testing.T, cp checkpoint.Checkpointer, be backend.Backend) {
	t.Helper()
	w := newWriter()
	p1 := newProc(t, Config{Subagents: []subagents.Subagent{w.spec()}}, cp, be, nil,
		start("s1", "researcher", "write"), call("c1", ToolCheck, obj{"task_id": "async_1", "wait": true}), llmtest.Turn{Text: "waiting"})
	p1.run(t, "go", nil)
	if r := record(t, p1.st, "async_1"); r.Status != StatusWaiting {
		t.Fatalf("saved record = %+v, want waiting", r)
	}
	p1.crash(t)
}

func TestResolveAfterRestart(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	crashWhileWaiting(t, cp, be)

	spy := &modeSpy{}
	w := &writer{child: llmtest.New(llmtest.Turn{Text: "wrote it"})}
	var mu sync.Mutex
	var settled []TaskRecord
	p2 := newProc(t, Config{Subagents: []subagents.Subagent{w.spec()}, OnSettled: func(_ context.Context, _ string, rec TaskRecord) {
		mu.Lock()
		defer mu.Unlock()
		settled = append(settled, rec)
	}}, cp, be, []agent.Middleware{spy.hooks()}, llmtest.Turn{Text: "noted"})
	approve := []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}}
	if err := p2.m.Resolve(context.Background(), "t", "async_1", approve); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, p2.m, "async_1")
	if err := p2.m.Resolve(context.Background(), "t", "async_1", approve); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("second Resolve = %v, want ErrNotWaiting", err)
	}
	p2.run(t, "next", nil)
	notes := notifications(p2.p) // process 1's announced wait, then the end
	if len(notes) != 2 || !strings.Contains(notes[1].Text(), "async_1 (researcher) completed.") || !strings.Contains(notes[1].Text(), "wrote it") {
		t.Fatalf("notifications = %+v", notes)
	}
	if w.runs.Load() != 1 {
		t.Fatalf("write_file ran %d times, want 1", w.runs.Load())
	}
	got, err := be.Read(context.Background(), "/out.txt", 0, 10)
	if err != nil || len(got.Lines) != 1 || got.Lines[0] != "hello" {
		t.Fatalf("backend file = %+v, %v", got, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(settled)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(settled) != 1 || settled[0].Status != StatusCompleted {
		t.Fatalf("OnSettled = %+v, want the completed episode", settled)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.depths) != 1 || spy.depths[0] != 1 {
		t.Fatalf("resolved task depths = %v, want [1]", spy.depths)
	}
}

func TestResolveAfterRestartWithoutCheckpointer(t *testing.T) {
	approve := []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}}
	unbound, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unbound.Resolve(context.Background(), "t", "async_1", approve); !errors.Is(err, ErrCannotReattach) || !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("Resolve without a bound agent = %v, want ErrCannotReattach", err)
	}
	p := newProc(t, Config{}, nil, testutil.Memory(t), nil)
	if err := p.m.Resolve(context.Background(), "t", "async_1", approve); !errors.Is(err, ErrCannotReattach) {
		t.Fatalf("Resolve without a Checkpointer = %v, want ErrCannotReattach", err)
	}
}

func TestResolveAfterRestartErrors(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	crashWhileWaiting(t, cp, be)
	p2 := newProc(t, Config{Subagents: []subagents.Subagent{newWriter().spec()}}, cp, be, nil, llmtest.Turn{Text: "ok"})
	ctx := context.Background()
	approve := []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}}
	if err := p2.m.Resolve(ctx, "t", "async_9", approve); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	if err := p2.m.Resolve(ctx, "other", "async_1", approve); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown thread: %v", err)
	}
	// A decision that does not answer the pending call is refused; the
	// task still waits and a good one then resumes it.
	if err := p2.m.Resolve(ctx, "t", "async_1", []agent.Decision{{ToolUseID: "nope", Type: agent.Approve}}); err == nil || errors.Is(err, ErrNotWaiting) {
		t.Fatalf("bad decision = %v, want a decision error", err)
	}
	// The record says the model was told the task waits; the reattached
	// task is still not settled, so a run of the thread keeps it.
	p2.run(t, "next", nil)
	if p2.m.jobs.Get("t", "async_1") == nil {
		t.Fatal("the reattached waiting task was released")
	}
	if err := p2.m.Resolve(ctx, "t", "async_1", approve); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, p2.m, "async_1")
}

func TestContinueAfterRestart(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	p1 := newProc(t, Config{Subagents: []subagents.Subagent{researcher(llmtest.New(llmtest.Turn{Text: "A"}))}}, cp, be, nil,
		start("s1", "researcher", "find A"), call("c1", ToolCheck, obj{"task_id": "async_1", "wait": true}), llmtest.Turn{Text: "got A"})
	p1.run(t, "go", nil)
	if r := record(t, p1.st, "async_1"); r.Status != StatusCompleted {
		t.Fatalf("saved record = %+v, want completed", r)
	}
	p1.crash(t)

	spy := &modeSpy{}
	child := llmtest.New(llmtest.Turn{Text: "B"})
	p2 := newProc(t, Config{Subagents: []subagents.Subagent{researcher(child)}}, cp, be, []agent.Middleware{spy.hooks()},
		update("u1", "async_1", "and B?"), llmtest.Turn{Text: "asked"}, llmtest.Turn{Text: "noted"})
	evs := p2.run(t, "more", nil)
	if res := result(t, evs, ToolUpdate); res.IsError {
		t.Fatalf("update_task = %+v", res)
	}
	waitEnded(t, p2.m, "async_1")
	p2.run(t, "and?", nil)
	notes := notifications(p2.p)
	if len(notes) != 1 || !strings.Contains(notes[0].Text(), "completed.") || !strings.Contains(notes[0].Text(), "B") {
		t.Fatalf("notifications = %+v", notes)
	}
	if len(child.Requests) != 1 {
		t.Fatalf("the continued child made %d requests, want 1", len(child.Requests))
	}
	text := requestText(child.Requests[0])
	for _, want := range []string{"find A", "A", "and B?"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the continued request lacks %q:\n%s", want, text)
		}
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.depths) != 1 || spy.depths[0] != 1 {
		t.Fatalf("continued task depths = %v, want [1]", spy.depths)
	}
}

func TestBindKeepsFirstAgent(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	cp1, cp2 := cpmemory.New(), cpmemory.New()
	s := state.New()
	addRecord(s, TaskRecord{ID: "async_1", ThreadID: "t", TaskThreadID: "t/async_1", SubagentType: "general-purpose", Status: StatusCompleted})
	if _, err := cp1.Save(context.Background(), "t", s, checkpoint.Version("")); err != nil {
		t.Fatal(err)
	}
	newAgent := func(cp checkpoint.Checkpointer) *agent.Agent {
		a, err := agent.New(agent.Config{Provider: llmtest.New(), Middleware: []agent.Middleware{m}, Checkpointer: cp})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a1 := newAgent(cp1)
	newAgent(cp2)
	if m.bound.Load() != a1 {
		t.Fatal("the middleware is not bound to the first agent")
	}
	// The record is found in the first agent's store: it is not waiting.
	err = m.Resolve(context.Background(), "t", "async_1", []agent.Decision{{ToolUseID: "x", Type: agent.Approve}})
	if !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("Resolve = %v, want ErrNotWaiting from the first agent's store", err)
	}
}

func TestResolveAfterRestartRecordedRunning(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	w := newWriter()
	p1 := newProc(t, Config{Subagents: []subagents.Subagent{w.spec()}}, cp, be, nil,
		start("s1", "researcher", "write"), llmtest.Turn{Text: "started"})
	p1.run(t, "go", nil)
	waitEnded(t, p1.m, "async_1")
	// The parent's run ended before the task started waiting.
	if r := record(t, p1.st, "async_1"); r.Status != StatusRunning {
		t.Fatalf("saved record = %+v, want running", r)
	}
	p1.crash(t)

	w2 := &writer{child: llmtest.New(llmtest.Turn{Text: "wrote it"})}
	p2 := newProc(t, Config{Subagents: []subagents.Subagent{w2.spec()}}, cp, be, nil)
	if err := p2.m.Resolve(context.Background(), "t", "async_1", []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}}); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, p2.m, "async_1")
	if out := outcomeOf(t, p2.m, p2.st, "async_1"); out.Status != StatusCompleted || out.Text != "wrote it" {
		t.Fatalf("outcome = %+v", out)
	}
	if w2.runs.Load() != 1 {
		t.Fatalf("write_file ran %d times, want 1", w2.runs.Load())
	}
}

// perThread is a BackendFactory that keeps one backend per thread, as the
// root preset's does.
type perThread struct {
	t  *testing.T
	mu sync.Mutex
	m  map[string]backend.Backend
}

func (p *perThread) get(id string) backend.Backend {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]backend.Backend{}
	}
	if b, ok := p.m[id]; ok {
		return b
	}
	b := testutil.Memory(p.t)
	p.m[id] = b
	return b
}

func TestResolveAfterRestartKeepsSyncedWorkspace(t *testing.T) {
	cp := cpmemory.New()
	ctx := context.Background()
	mk := func(sub subagents.Subagent, f *perThread, turns ...llmtest.Turn) (*Middleware, *agent.Agent) {
		m, err := New(Config{Subagents: []subagents.Subagent{sub}, DisableStopReminder: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close(ctx) })
		a, err := agent.New(agent.Config{Provider: llmtest.New(turns...), Middleware: []agent.Middleware{m}, PermissionMode: permission.ModeDefault,
			Checkpointer: cp, BackendFactory: f.get, SyncWorkspace: true})
		if err != nil {
			t.Fatal(err)
		}
		return m, a
	}
	prompt := func(a *agent.Agent, text string) {
		th, err := a.Thread(ctx, "t")
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range th.Prompt(ctx, text) {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	f1 := &perThread{t: t}
	if err := f1.get("t").Write(ctx, "/keep.txt", []byte("precious")); err != nil {
		t.Fatal(err)
	}
	m1, a1 := mk(newWriter().spec(), f1, start("s1", "researcher", "write"), call("c1", ToolCheck, obj{"task_id": "async_1", "wait": true}), llmtest.Turn{Text: "waiting"})
	prompt(a1, "go")
	_ = m1.Close(ctx)

	// Process 2's backends start empty: the thread's files are only in its
	// saved workspace.
	f2 := &perThread{t: t}
	w2 := &writer{child: llmtest.New(llmtest.Turn{Text: "wrote it"})}
	m2, a2 := mk(w2.spec(), f2, llmtest.Turn{Text: "ok"})
	if err := m2.Resolve(ctx, "t", "async_1", []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}}); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, m2, "async_1")
	prompt(a2, "next")
	for _, name := range []string{"/keep.txt", "/out.txt"} {
		if _, err := f2.get("t").Read(ctx, name, 0, 10); err != nil {
			t.Fatalf("%s is missing from the thread's workspace after a Resolve from a new process: %v", name, err)
		}
	}
	// And from the snapshot the run saved.
	f3 := &perThread{t: t}
	m3, a3 := mk(newWriter().spec(), f3, llmtest.Turn{Text: "ok"})
	_ = m3
	prompt(a3, "again")
	if _, err := f3.get("t").Read(ctx, "/keep.txt", 0, 10); err != nil {
		t.Fatalf("the saved workspace lost /keep.txt: %v", err)
	}
}

// marker is a tool writing /mark.txt to the call's backend.
var marker = tool.New("mark", "Marks.", func(ctx context.Context, _ struct{}, c *tool.Call) (tool.Result, error) {
	return tool.Text("marked"), c.Backend.Write(ctx, "/mark.txt", []byte("x"))
}, tool.WithReadOnly())

func TestReattachRunsOnRunBackend(t *testing.T) {
	cp := cpmemory.New()
	cfgBe, runBe := testutil.Memory(t), testutil.Memory(t)
	b := newBlocker(call("n1", "noop", obj{}))
	p1 := newProc(t, Config{Subagents: []subagents.Subagent{worker(b, "")}}, cp, cfgBe, nil,
		start("s1", "researcher", "slow research"), llmtest.Turn{Text: "started"})
	p1.runBackend = runBe
	p1.run(t, "go", nil)
	for range 2 {
		<-b.entered
	}
	p1.crash(t)

	spec := researcher(llmtest.New(call("m1", "mark", obj{}), llmtest.Turn{Text: "done"}))
	spec.Tools = []tool.Tool{noop, marker}
	p2 := newProc(t, Config{Subagents: []subagents.Subagent{spec}}, cp, cfgBe, nil, llmtest.Turn{Text: "checking"})
	p2.runBackend = runBe
	p2.run(t, "status?", nil)
	waitEnded(t, p2.m, "async_1")
	ctx := context.Background()
	if _, err := runBe.Read(ctx, "/mark.txt", 0, 10); err != nil {
		t.Fatalf("the reattached task did not work on the run's backend: %v", err)
	}
	if _, err := cfgBe.Read(ctx, "/mark.txt", 0, 10); err == nil {
		t.Fatal("the reattached task worked on Config.Backend, not the run's")
	}
}

func TestResolveAfterRestartRunsUnderBoundAgent(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	crashWhileWaiting(t, cp, be)
	spy := &modeSpy{}
	w := &writer{child: llmtest.New(llmtest.Turn{Text: "wrote it"})}
	p2 := newProc(t, Config{Subagents: []subagents.Subagent{w.spec()}}, cp, be, []agent.Middleware{spy.hooks()})
	// The host resolves from inside some other agent's deep run.
	other, err := agent.New(agent.Config{Provider: llmtest.New()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := agenttest.Context(context.Background(), agent.RunInfo{Agent: other, ThreadID: "elsewhere", Depth: 3})
	if err := p2.m.Resolve(ctx, "t", "async_1", []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}}); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, p2.m, "async_1")
	if w.runs.Load() != 1 {
		t.Fatalf("write_file ran %d times, want 1", w.runs.Load())
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	// The spy is in the bound agent's stack: the task ran under it, at
	// Depth 1, not under the caller's run.
	if len(spy.depths) != 1 || spy.depths[0] != 1 {
		t.Fatalf("resolved task depths = %v, want [1] under the bound agent", spy.depths)
	}
}

func TestResolveRefusesTaskThread(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	p := newProc(t, Config{}, cp, be, nil)
	err := p.m.Resolve(context.Background(), "t/async_1", "async_1", []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}})
	if !errors.Is(err, ErrCannotReattach) {
		t.Fatalf("Resolve on a task's thread = %v, want ErrCannotReattach", err)
	}
	if !isTaskThread("t/async_2/x") || isTaskThread("async_1") || isTaskThread("t/async_x") || isTaskThread("t/async_") {
		t.Fatal("isTaskThread misreads thread ids")
	}
}

func TestReattachRespectsMaxRunning(t *testing.T) {
	cp, be := cpmemory.New(), testutil.Memory(t)
	b := newBlocker(call("n1", "noop", obj{}))
	p1 := newProc(t, Config{Subagents: []subagents.Subagent{worker(b, "")}}, cp, be, nil,
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "s1", Name: ToolStart, Input: obj{"subagent_type": "researcher", "description": "one"}},
			{ID: "s2", Name: ToolStart, Input: obj{"subagent_type": "researcher", "description": "two"}},
		}}, llmtest.Turn{Text: "started"})
	p1.run(t, "go", nil)
	for range 2 {
		<-b.entered // both tasks called their model
	}
	p1.crash(t)

	calls := &settledCalls{}
	g := newGate(llmtest.Turn{Text: "done"})
	p2 := newProc(t, Config{Subagents: []subagents.Subagent{worker(g, "")}, MaxRunning: 1, OnSettled: calls.hook}, cp, be, nil,
		llmtest.Turn{Text: "checking"})
	p2.run(t, "status?", nil)
	g.waitEntered(t, 1)
	time.Sleep(20 * time.Millisecond)
	select {
	case <-g.entered:
		t.Fatal("a task over the running limit was started")
	default:
	}
	// The tasks started concurrently, so either may be recorded first and
	// take the only slot; the other is lost.
	var kept string
	lost := 0
	for _, r := range Tasks(p2.st) {
		switch r.Status {
		case StatusLost:
			lost++
		case StatusRunning:
			kept = r.ID
		}
	}
	if lost != 1 || kept == "" {
		t.Fatalf("records = %+v, want one running and one lost", Tasks(p2.st))
	}
	g.release()
	waitEnded(t, p2.m, kept)
	for _, rec := range calls.waitCalls(t, 1) {
		if rec.ID != kept {
			t.Fatalf("OnSettled reported %+v, a task that was never taken over", rec)
		}
	}
}
