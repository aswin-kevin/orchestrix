package async

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/agenttest"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// wantWaitingNotice is the notification of a task waiting to write a file.
const wantWaitingNotice = "<task-notification>Background task async_1 (researcher) is waiting for a decision on: write_file. Only the user can answer it; carry on with other work or tell the user.</task-notification>"

// writer is a researcher whose write_file tool needs approval in the default
// mode; each run of the tool writes /out.txt to the call's backend and is
// counted.
type writer struct {
	child *llmtest.Scripted
	runs  atomic.Int32
}

// newWriter returns a writer whose model calls write_file, then answers
// with each of after.
func newWriter(after ...llmtest.Turn) *writer {
	turns := append([]llmtest.Turn{call("w1", "write_file", obj{})}, after...)
	return &writer{child: llmtest.New(turns...)}
}

func (w *writer) spec() subagents.Subagent {
	writeFile := tool.New("write_file", "Writes.", func(ctx context.Context, _ struct{}, c *tool.Call) (tool.Result, error) {
		w.runs.Add(1)
		if err := c.Backend.Write(ctx, "/out.txt", []byte("hello")); err != nil {
			return tool.Result{}, err
		}
		return tool.Text("written"), nil
	})
	spec := researcher(w.child)
	spec.Tools = []tool.Tool{writeFile}
	return spec
}

// waitingHarness runs a parent whose task async_1 now waits to write a file;
// the parent's turns after its first run are more.
func waitingHarness(t *testing.T, cfg Config, w *writer, before []agent.Middleware, more ...llmtest.Turn) *harness {
	t.Helper()
	cfg.Subagents = []subagents.Subagent{w.spec()}
	cfg.DisableStopReminder = true
	turns := append([]llmtest.Turn{start("s1", "researcher", "write"), {Text: "started"}}, more...)
	h := newHarnessWith(t, cfg, before, turns...)
	h.be = testutil.Memory(t)
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	return h
}

// approveAll approves every request of the waiting record id.
func approveAll(t *testing.T, r TaskRecord) []agent.Decision {
	t.Helper()
	if r.Pending == nil || len(r.Pending.Requests) == 0 {
		t.Fatalf("record %s waits for nothing: %+v", r.ID, r)
	}
	var out []agent.Decision
	for _, req := range r.Pending.Requests {
		out = append(out, agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Approve})
	}
	return out
}

// liveRecord returns the record id as the middleware sees it now.
func liveRecord(t *testing.T, h *harness, id string) TaskRecord {
	t.Helper()
	for _, r := range h.m.updateRecords(h.st, "t", nil) {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no record %s", id)
	return TaskRecord{}
}

func TestWaitingSurfacesPending(t *testing.T) {
	w := newWriter()
	h := waitingHarness(t, Config{}, w, nil, llmtest.Turn{Text: "ok"})
	h.run(t, context.Background(), "next", nil)
	r := record(t, h.st, "async_1")
	if r.Status != StatusWaiting || r.Pending == nil || r.Pending.Requests[0].Tool != "write_file" {
		t.Fatalf("record = %+v", r)
	}
	notes := notifications(h.p)
	if len(notes) != 1 || notes[0].Text() != wantWaitingNotice {
		t.Fatalf("notifications = %+v", notes)
	}
	if w.runs.Load() != 0 {
		t.Fatal("the unapproved call ran")
	}
}

func TestParentCannotResolveWaiting(t *testing.T) {
	w := newWriter()
	h := waitingHarness(t, Config{}, w, nil,
		update("u1", "async_1", "just write it"),
		call("c1", ToolCheck, obj{"task_id": "async_1"}),
		llmtest.Turn{Text: "ok"})
	fb := h.m.runner.(*localRunner).fallback
	pendingNow := func() *agent.Interrupt {
		s, _, err := fb.Load(context.Background(), "t/async_1")
		if err != nil {
			t.Fatalf("task thread: %v", err)
		}
		p, ok := agent.Pending(s)
		if !ok {
			t.Fatal("the task thread holds no pending action")
		}
		return p
	}
	before := pendingNow()
	evs, _ := h.run(t, context.Background(), "next", nil)
	if res := result(t, evs, ToolUpdate); !res.IsError || res.Flatten() != "cannot update task async_1: it is waiting for a decision only the user can make" {
		t.Fatalf("update_task = %+v", res)
	}
	if res := result(t, evs, ToolCheck).Flatten(); res != "Task async_1 (researcher): waiting for a decision on: write_file." {
		t.Fatalf("check_task = %q", res)
	}
	if after := pendingNow(); !reflect.DeepEqual(before, after) {
		t.Fatalf("the parent's turn changed the pending action: %+v, then %+v", before, after)
	}
	if r := record(t, h.st, "async_1"); r.Status != StatusWaiting || w.runs.Load() != 0 {
		t.Fatalf("record = %+v, write ran %d times", r, w.runs.Load())
	}
	for _, tl := range h.m.Tools() {
		name := tl.Name()
		for _, word := range []string{"resolve", "approve", "decide"} {
			if strings.Contains(name, word) {
				t.Errorf("tool %s lets the model answer a pending action", name)
			}
		}
	}
}

func TestWaitingNotReleased(t *testing.T) {
	w := newWriter(llmtest.Turn{Text: "done"})
	h := waitingHarness(t, Config{}, w, nil, llmtest.Turn{Text: "a"}, llmtest.Turn{Text: "b"}, llmtest.Turn{Text: "c"})
	for range 3 {
		h.run(t, context.Background(), "more", nil)
	}
	r := record(t, h.st, "async_1")
	if r.Status != StatusWaiting {
		t.Fatalf("record = %+v", r)
	}
	if h.m.jobs.Get("t", "async_1") == nil {
		t.Fatal("the waiting task was released")
	}
	if err := h.m.Resolve(context.Background(), "t", "async_1", approveAll(t, r)); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, h.m, "async_1")
}

func TestHostResolveResumes(t *testing.T) {
	w := newWriter(llmtest.Turn{Text: "wrote it"})
	h := waitingHarness(t, Config{}, w, nil, llmtest.Turn{Text: "ok"}, llmtest.Turn{Text: "noted"})
	h.run(t, context.Background(), "next", nil) // announces the wait
	r := record(t, h.st, "async_1")
	if err := h.m.Resolve(context.Background(), "t", "async_1", approveAll(t, r)); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, h.m, "async_1")
	h.run(t, context.Background(), "after", nil)
	notes := notifications(h.p) // the wait's, then the end's
	if len(notes) != 2 || !strings.Contains(notes[1].Text(), "Background task async_1 (researcher) completed.") || !strings.Contains(notes[1].Text(), "wrote it") {
		t.Fatalf("notifications = %+v", notes)
	}
	if r := record(t, h.st, "async_1"); r.Status != StatusCompleted || r.Result != "wrote it" || r.Pending != nil || !r.Notified {
		t.Fatalf("record = %+v", r)
	}
	if w.runs.Load() != 1 {
		t.Fatalf("write_file ran %d times, want 1", w.runs.Load())
	}
	got, err := h.be.Read(context.Background(), "/out.txt", 0, 10)
	if err != nil || len(got.Lines) != 1 || got.Lines[0] != "hello" {
		t.Fatalf("backend file = %+v, %v", got, err)
	}
	reqs := w.child.Requests
	if len(reqs) != 2 || !strings.Contains(requestText(reqs[1]), "written") {
		t.Fatalf("the resumed episode's request lacks the tool result: %d requests", len(reqs))
	}
}

func TestHostResolvedEpisodeIsNested(t *testing.T) {
	spy := &modeSpy{}
	w := newWriter(llmtest.Turn{Text: "wrote it"})
	h := waitingHarness(t, Config{}, w, []agent.Middleware{spy.hooks()})
	r := liveRecord(t, h, "async_1")
	if err := h.m.Resolve(context.Background(), "t", "async_1", approveAll(t, r)); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, h.m, "async_1")
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.depths) != 2 || spy.depths[0] != 1 || spy.depths[1] != 1 {
		t.Fatalf("task depths = %v, want [1 1]", spy.depths)
	}
}

func TestResolveErrors(t *testing.T) {
	w := newWriter(llmtest.Turn{Text: "wrote it"})
	h := waitingHarness(t, Config{}, w, nil)
	ctx := context.Background()
	if err := h.m.Resolve(ctx, "t", "async_9", nil); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	if err := h.m.Resolve(ctx, "other", "async_1", nil); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("another thread's task: %v", err)
	}
	// Decisions that do not answer the pending action are refused and the
	// task keeps waiting.
	for _, bad := range [][]agent.Decision{
		nil,
		{{ToolUseID: "nope", Type: agent.Approve}},
		{{ToolUseID: "w1", Type: agent.Approve}, {ToolUseID: "w1", Type: agent.Approve}},
	} {
		if err := h.m.Resolve(ctx, "t", "async_1", bad); err == nil || errors.Is(err, ErrNotWaiting) {
			t.Fatalf("Resolve(%+v) = %v, want a decision error", bad, err)
		}
	}
	r := liveRecord(t, h, "async_1")
	if r.Status != StatusWaiting {
		t.Fatalf("record after refused decisions = %+v", r)
	}
	if err := h.m.Resolve(ctx, "t", "async_1", approveAll(t, r)); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, h.m, "async_1")
	if err := h.m.Resolve(ctx, "t", "async_1", approveAll(t, r)); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("Resolve of a completed task = %v, want ErrNotWaiting", err)
	}
}

func TestResolveRaceRunsOnce(t *testing.T) {
	w := newWriter(llmtest.Turn{Text: "wrote it"})
	h := waitingHarness(t, Config{}, w, nil)
	decisions := approveAll(t, liveRecord(t, h, "async_1"))
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = h.m.Resolve(context.Background(), "t", "async_1", decisions) })
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrNotWaiting):
			t.Fatalf("losing Resolve = %v, want ErrNotWaiting", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d Resolve calls won, want 1", wins)
	}
	waitEnded(t, h.m, "async_1")
	if w.runs.Load() != 1 {
		t.Fatalf("the approved call ran %d times, want 1", w.runs.Load())
	}
}

// settledCalls collects OnSettled calls.
type settledCalls struct {
	mu    sync.Mutex
	calls []TaskRecord
	tids  []string
	info  []bool
}

func (s *settledCalls) hook(ctx context.Context, threadID string, rec TaskRecord) {
	_, ok := agent.Info(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, rec)
	s.tids = append(s.tids, threadID)
	s.info = append(s.info, ok)
}

func (s *settledCalls) get() []TaskRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TaskRecord(nil), s.calls...)
}

// waitCalls waits until s holds n calls.
func (s *settledCalls) waitCalls(t *testing.T, n int) []TaskRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := s.get(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("OnSettled called %d times, want %d", len(s.get()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOnSettledEndAndWaiting(t *testing.T) {
	calls := &settledCalls{}
	w := newWriter(llmtest.Turn{Text: "wrote it"})
	h := waitingHarness(t, Config{OnSettled: calls.hook}, w, nil)
	got := calls.waitCalls(t, 1)
	first := got[0]
	if first.Status != StatusWaiting || first.Pending == nil || first.ID != "async_1" || first.ThreadID != "t" ||
		first.TaskThreadID != "t/async_1" || first.SubagentType != "researcher" || first.Description != "write" ||
		first.Started.IsZero() || first.Notified {
		t.Fatalf("first OnSettled record = %+v", first)
	}
	if err := h.m.Resolve(context.Background(), "t", "async_1", approveAll(t, first)); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	got = calls.waitCalls(t, 2)
	second := got[1]
	if second.Status != StatusCompleted || second.Pending != nil || second.Result != "wrote it" || second.Ended.IsZero() || second.Notified {
		t.Fatalf("second OnSettled record = %+v", second)
	}
	time.Sleep(20 * time.Millisecond)
	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.calls) != 2 || calls.tids[0] != "t" || calls.tids[1] != "t" {
		t.Fatalf("OnSettled calls = %+v on threads %v, want 2 on t", calls.calls, calls.tids)
	}
	// The first episode started in start_task's call; the second in the
	// host's Resolve, whose ctx carries no run.
	if !calls.info[0] || calls.info[1] {
		t.Fatalf("agent.Info present = %v, want [true false]", calls.info)
	}
}

func TestOnSettledPanicIsRecovered(t *testing.T) {
	var called atomic.Bool
	hook := func(context.Context, string, TaskRecord) { called.Store(true); panic("boom") }
	w := newWriter()
	h := waitingHarness(t, Config{OnSettled: hook}, w, nil, llmtest.Turn{Text: "ok"})
	deadline := time.Now().Add(5 * time.Second)
	for !called.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !called.Load() {
		t.Fatal("OnSettled was not called")
	}
	h.run(t, context.Background(), "next", nil)
	if r := record(t, h.st, "async_1"); r.Status != StatusWaiting {
		t.Fatalf("record = %+v", r)
	}
}

func TestOnEventStreamsTaskEvents(t *testing.T) {
	var mu sync.Mutex
	var evs []agent.Event
	var threads []string
	onEvent := func(threadID, id string, ev agent.Event) {
		if id != "async_1" {
			return
		}
		mu.Lock()
		threads = append(threads, threadID)
		evs = append(evs, ev)
		mu.Unlock()
	}
	child := llmtest.New(llmtest.Turn{Text: "child says hi"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(child)}, OnEvent: onEvent, DisableStopReminder: true},
		start("s1", "researcher", "x"), llmtest.Turn{Text: "started"})
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	mu.Lock()
	defer mu.Unlock()
	text := false
	for _, ev := range evs {
		if ev.Kind == agent.KindText && strings.Contains(ev.Text, "child says hi") {
			text = true
		}
	}
	if !text || len(evs) == 0 || evs[len(evs)-1].Kind != agent.KindFinal {
		t.Fatalf("events = %+v; want the child's text and a final KindFinal", evs)
	}
	for _, tid := range threads {
		if tid != h.thread {
			t.Fatalf("OnEvent threads = %v, want every event on the parent's thread %q", threads, h.thread)
		}
	}
}

func TestApproverStaysInBand(t *testing.T) {
	calls := &settledCalls{}
	w := newWriter(llmtest.Turn{Text: "wrote it"})
	m, err := New(Config{Subagents: []subagents.Subagent{w.spec()}, OnSettled: calls.hook, DisableStopReminder: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	var asked atomic.Int32
	p := llmtest.New(start("s1", "researcher", "write"), llmtest.Turn{Text: "started"})
	a, err := agent.New(agent.Config{Provider: p, Middleware: []agent.Middleware{m}, PermissionMode: permission.ModeDefault,
		Approver: func(context.Context, permission.Request, string) permission.Decision {
			asked.Add(1)
			return permission.Allowed()
		}})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{m: m, a: a, p: p, st: state.New(), thread: "t", be: testutil.Memory(t)}
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, m, "async_1")
	calls.waitCalls(t, 1)
	time.Sleep(20 * time.Millisecond)
	if got := calls.get(); len(got) != 1 || got[0].Status != StatusCompleted {
		t.Fatalf("OnSettled calls = %+v, want one completed", got)
	}
	if asked.Load() != 1 || w.runs.Load() != 1 {
		t.Fatalf("approver asked %d times, write ran %d times; want 1 and 1", asked.Load(), w.runs.Load())
	}
}

func TestCancelWaitingTask(t *testing.T) {
	calls := &settledCalls{}
	hd := waitingHandle()
	run := funcRunner(func(context.Context, Request) (Handle, error) { return hd, nil })
	h := newHarness(t, Config{Runner: run, OnSettled: calls.hook, DisableStopReminder: true},
		start("s1", gp, "x"), llmtest.Turn{Text: "started"},
		call("c1", ToolCancel, obj{"task_id": "async_1"}), llmtest.Turn{Text: "ok"})
	h.run(t, context.Background(), "go", nil)
	began := time.Now()
	evs, _ := h.run(t, context.Background(), "cancel", nil)
	if took := time.Since(began); took > time.Second {
		t.Fatalf("cancelling a waiting task took %v", took)
	}
	if res := result(t, evs, ToolCancel); res.IsError || res.Flatten() != "Cancelled task async_1." {
		t.Fatalf("cancel_task = %+v", res)
	}
	r := record(t, h.st, "async_1")
	if r.Status != StatusCancelled || !r.Notified || r.Pending != nil || r.Ended.IsZero() {
		t.Fatalf("record = %+v", r)
	}
	if j := h.m.jobs.Get("t", "async_1"); j != nil && !j.Notified() {
		t.Fatal("the cancelled task's job is not marked notified")
	}
	if hd.stops.Load() != 1 {
		t.Fatalf("Stop called %d times, want 1", hd.stops.Load())
	}
	if err := h.m.Resolve(context.Background(), "t", "async_1", nil); err == nil {
		t.Fatal("Resolve of a cancelled task succeeded")
	}
	// The host is told the waiting task ended.
	got := calls.waitCalls(t, 2)
	if got[0].Status != StatusWaiting || got[1].Status != StatusCancelled {
		t.Fatalf("OnSettled calls = %+v, want waiting then cancelled", got)
	}
}

func TestCloseAnnouncesCancelledWaiting(t *testing.T) {
	run := funcRunner(func(context.Context, Request) (Handle, error) { return waitingHandle(), nil })
	h := newHarness(t, Config{Runner: run, DisableStopReminder: true},
		start("s1", gp, "x"), llmtest.Turn{Text: "started"}, llmtest.Turn{Text: "ok"}, llmtest.Turn{Text: "ok"})
	h.run(t, context.Background(), "go", nil)
	h.run(t, context.Background(), "next", nil) // announces the wait
	if r := record(t, h.st, "async_1"); r.Status != StatusWaiting || !r.Notified {
		t.Fatalf("record = %+v", r)
	}
	if err := h.m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.run(t, context.Background(), "after close", nil)
	notes := notifications(h.p) // the wait's, then the cancellation's
	if len(notes) != 2 || notes[1].Text() != "<task-notification>Background task async_1 (general-purpose) cancelled.</task-notification>" {
		t.Fatalf("notifications = %+v", notes)
	}
}

// resHandle is a waiting ctlHandle that is a Resolver: Resolve returns a
// handle that already completed.
type resHandle struct {
	*ctlHandle
	calls atomic.Int32
}

func (r *resHandle) Resolve(context.Context, []agent.Decision) (Handle, error) {
	r.calls.Add(1)
	return ended(Outcome{Status: StatusCompleted, Text: "resolved"}), nil
}

func TestWaitingSurvivesEviction(t *testing.T) {
	waiting := &resHandle{ctlHandle: waitingHandle()}
	var n atomic.Int32
	run := funcRunner(func(context.Context, Request) (Handle, error) {
		if n.Add(1) == 1 {
			return waiting, nil
		}
		return ended(Outcome{Status: StatusCompleted, Text: "quick"}), nil
	})
	h := newHarness(t, Config{Runner: run, MaxFinished: 1, DisableStopReminder: true},
		start("s1", gp, "a"), llmtest.Turn{Text: "started"},
		start("s2", gp, "b"), llmtest.Turn{Text: "started"},
		start("s3", gp, "c"), llmtest.Turn{Text: "started"},
		llmtest.Turn{Text: "ok"})
	for range 4 {
		h.run(t, context.Background(), "go", nil)
	}
	if h.m.jobs.Get("t", "async_1") == nil {
		t.Fatal("the waiting task was evicted")
	}
	// async_2's record was pruned once announced (MaxFinished 1), not lost.
	for _, r := range Tasks(h.st) {
		if r.ID != "async_1" && (r.Status != StatusCompleted || !r.Notified) {
			t.Fatalf("record %s = %+v", r.ID, r)
		}
	}
	if r := record(t, h.st, "async_3"); r.Status != StatusCompleted {
		t.Fatalf("record async_3 = %+v", r)
	}
	r := record(t, h.st, "async_1")
	if r.Status != StatusWaiting {
		t.Fatalf("record = %+v", r)
	}
	if err := h.m.Resolve(context.Background(), "t", "async_1", []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}}); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if waiting.calls.Load() != 1 {
		t.Fatalf("Resolve reached the Handle %d times", waiting.calls.Load())
	}
}

func TestResolvedRecordIsRunningThenAnnounced(t *testing.T) {
	next := newCtlHandle(Outcome{Status: StatusCancelled})
	waiting := &gateResolver{ctlHandle: waitingHandle(), next: next}
	run := funcRunner(func(context.Context, Request) (Handle, error) { return waiting, nil })
	h := newHarness(t, Config{Runner: run, DisableStopReminder: true},
		start("s1", gp, "a"), llmtest.Turn{Text: "started"}, llmtest.Turn{Text: "ok"},
		llmtest.Turn{Text: "ok"}, llmtest.Turn{Text: "ok"})
	h.run(t, context.Background(), "go", nil)
	h.run(t, context.Background(), "next", nil) // announces the wait
	if err := h.m.Resolve(context.Background(), "t", "async_1", []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}}); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	h.run(t, context.Background(), "while it runs", nil)
	if r := record(t, h.st, "async_1"); r.Status != StatusRunning || r.Pending != nil || r.Notified {
		t.Fatalf("record while the resolved episode runs = %+v", r)
	}
	next.end(Outcome{Status: StatusCompleted, Text: "resolved"})
	waitEnded(t, h.m, "async_1")
	h.run(t, context.Background(), "after", nil)
	notes := notifications(h.p) // the wait's, then the end's
	if len(notes) != 2 || !strings.Contains(notes[1].Text(), "completed") {
		t.Fatalf("notifications = %+v", notes)
	}
}

// gateResolver is a waiting ctlHandle whose Resolve returns next.
type gateResolver struct {
	*ctlHandle
	next *ctlHandle
}

func (g *gateResolver) Resolve(context.Context, []agent.Decision) (Handle, error) { return g.next, nil }

func TestContinueAfterCancelledWaitingReleasedDoesNotRunPending(t *testing.T) {
	w := newWriter(llmtest.Turn{Text: "continued"})
	h := waitingHarness(t, Config{}, w, nil,
		call("c1", ToolCancel, obj{"task_id": "async_1"}), llmtest.Turn{Text: "cancelled"},
		llmtest.Turn{Text: "idle"},
		update("u1", "async_1", "go on"), llmtest.Turn{Text: "asked"})
	h.run(t, context.Background(), "cancel it", nil)
	h.run(t, context.Background(), "idle", nil)
	if h.m.jobs.Get("t", "async_1") != nil {
		t.Fatal("the cancelled task is still held")
	}
	evs, _ := h.run(t, context.Background(), "go on", nil)
	// As on the held path: the task was cancelled while it waited.
	res := result(t, evs, ToolUpdate)
	if !res.IsError || !strings.Contains(res.Flatten(), "cancelled while it waited") {
		t.Fatalf("update_task = %+v", res)
	}
	if w.runs.Load() != 0 {
		t.Fatalf("the unapproved call ran %d times after the task was cancelled (update_task = %+v)", w.runs.Load(), res)
	}
}

func TestLocalResolveAppliesStricterParentMode(t *testing.T) {
	w := newWriter(llmtest.Turn{Text: "could not write"})
	h := waitingHarness(t, Config{}, w, nil, llmtest.Turn{Text: "ok"})
	if err := agent.SetPermissionMode(h.st, permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	h.run(t, context.Background(), "plan now", nil)
	r := record(t, h.st, "async_1")
	if err := h.m.Resolve(context.Background(), "t", "async_1", approveAll(t, r)); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	waitEnded(t, h.m, "async_1")
	if w.runs.Load() != 0 {
		t.Fatal("an approved write ran while the parent is in plan mode")
	}
}

func TestAttachReadsPendingFromThread(t *testing.T) {
	w := newWriter()
	h := waitingHarness(t, Config{}, w, nil)
	lr := h.m.runner.(*localRunner)
	rec := liveRecord(t, h, "async_1")
	ctx, _ := agenttest.Context(context.Background(), agent.RunInfo{Agent: h.a, ThreadID: "t", Depth: 0})
	ctx = withFollowUp(ctx, followUp{backend: h.be})
	stale := rec
	stale.Pending = &agent.Interrupt{Requests: []agent.ActionRequest{{ToolUseID: "stale", Tool: "write_file"}}}
	hd, err := lr.Attach(ctx, stale)
	if err != nil {
		t.Fatalf("Attach = %v", err)
	}
	out, ok := hd.Outcome()
	if !ok || out.Status != StatusWaiting || out.Pending == nil || out.Pending.Requests[0].ToolUseID != "w1" {
		t.Fatalf("attached outcome = %+v, %v; want the thread's pending call w1", out, ok)
	}
	cancelled := rec
	cancelled.Status, cancelled.Pending = StatusCancelled, nil
	if _, err := lr.Attach(ctx, cancelled); !errors.Is(err, errCancelledWaiting) {
		t.Fatalf("Attach of a task cancelled while it waited = %v, want errCancelledWaiting", err)
	}
}
