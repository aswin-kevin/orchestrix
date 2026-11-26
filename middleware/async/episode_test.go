package async

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/agenttest"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	cpmemory "github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
)

const wantFollowUp = "<task-update>\nand B?\n</task-update>"

// roleTexts renders the messages of req as "role: text" lines.
func roleTexts(req llm.Request) []string {
	out := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		out[i] = string(m.Role) + ": " + m.Text()
	}
	return out
}

// notificationsFor counts the stored notifications of task id.
func notificationsFor(s *state.State, id string) int {
	n := 0
	for _, m := range storedNotifications(s) {
		if m.Meta[NotificationMetaKey] == id {
			n++
		}
	}
	return n
}

// continueHistory runs a parent whose task answers "A", then is continued
// with "and B?" and answers "B". It returns the harness after the run that
// announced the second answer and the child's provider.
func continueHistory(t *testing.T, cfg Config, cp checkpoint.Checkpointer, extra ...llmtest.Turn) (*harness, *llmtest.Scripted) {
	t.Helper()
	child := llmtest.New(llmtest.Turn{Text: "A"}, llmtest.Turn{Text: "B"})
	cfg.Subagents = []subagents.Subagent{researcher(child)}
	cfg.DisableStopReminder = true
	turns := append([]llmtest.Turn{
		start("s1", "researcher", "find A"), {Text: "started"},
	}, extra...)
	turns = append(turns,
		update("u1", "async_1", "and B?"), llmtest.Turn{Text: "asked"},
		llmtest.Turn{Text: "done"},
	)
	h := newHarnessSaved(t, cfg, cp, nil, turns...)
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	for range len(extra) {
		h.run(t, context.Background(), "more", nil)
	}
	evs, _ := h.run(t, context.Background(), "ask more", nil)
	if res := result(t, evs, ToolUpdate); res.IsError || res.Flatten() != "Task async_1 continues with your message; you will be notified when it finishes." {
		t.Fatalf("update_task = %+v", res)
	}
	if r := record(t, h.st, "async_1"); r.Status != StatusRunning && r.Status != StatusCompleted {
		t.Fatalf("record after update_task = %+v", r)
	}
	waitEnded(t, h.m, "async_1")
	h.run(t, context.Background(), "next", nil)
	return h, child
}

func TestContinueKeepsHistory(t *testing.T) {
	// MaxRunning 1: the ended task does not count against the limit when it
	// is continued (it is the thread's only task).
	h, child := continueHistory(t, Config{MaxRunning: 1}, nil)
	reqs := child.Requests
	if len(reqs) != 2 {
		t.Fatalf("%d child requests, want 2", len(reqs))
	}
	got := strings.Join(roleTexts(reqs[1]), "\n")
	want := strings.Join([]string{"user: find A", "assistant: A", "user: " + wantFollowUp}, "\n")
	if got != want {
		t.Fatalf("second episode's messages:\n%s\nwant:\n%s", got, want)
	}
	r := record(t, h.st, "async_1")
	if r.Status != StatusCompleted || r.Result != "B" || r.Ended.IsZero() || !r.Notified {
		t.Fatalf("record = %+v", r)
	}
	if n := notificationsFor(h.st, "async_1"); n != 2 {
		t.Fatalf("%d notifications of async_1, want 2", n)
	}
}

func TestContinueSavesTaskThread(t *testing.T) {
	cp := cpmemory.New()
	var firstVersion checkpoint.Version
	child := llmtest.New(llmtest.Turn{Text: "A"}, llmtest.Turn{Text: "B"})
	h := newHarnessSaved(t, Config{Subagents: []subagents.Subagent{researcher(child)}, DisableStopReminder: true}, cp, nil,
		start("s1", "researcher", "find A"), llmtest.Turn{Text: "started"},
		update("u1", "async_1", "and B?"), llmtest.Turn{Text: "asked"},
	)
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	s, v, err := cp.Load(context.Background(), "t/async_1")
	if err != nil {
		t.Fatalf("task thread after the first episode: %v", err)
	}
	if n := len(s.Messages()); n != 2 {
		t.Fatalf("first episode saved %d messages, want 2", n)
	}
	firstVersion = v
	h.run(t, context.Background(), "ask more", nil)
	waitEnded(t, h.m, "async_1")
	s, v, err = cp.Load(context.Background(), "t/async_1")
	if err != nil {
		t.Fatalf("task thread after the second episode: %v", err)
	}
	if n := len(s.Messages()); n != 4 {
		t.Fatalf("task thread holds %d messages, want 4", n)
	}
	gen1, saves1, ok1 := checkpoint.ParseVersion(firstVersion)
	gen2, saves2, ok2 := checkpoint.ParseVersion(v)
	if !ok1 || !ok2 || gen1 != gen2 || saves2 <= saves1 {
		t.Fatalf("versions %q then %q; want the second later in the same generation", firstVersion, v)
	}
	if fb := h.m.runner.(*localRunner).fallback; fb != nil {
		if _, _, err := fb.Load(context.Background(), "t/async_1"); !errors.Is(err, checkpoint.ErrNotFound) {
			t.Fatalf("the fallback store holds the task thread (%v); the parent's Checkpointer should", err)
		}
	}
}

func TestContinueAfterRelease(t *testing.T) {
	// Two more parent runs: the first announces the end, the next releases
	// the thread's entry, so the job is no longer held.
	h, child := continueHistory(t, Config{}, nil, llmtest.Turn{Text: "noted"}, llmtest.Turn{Text: "idle"})
	reqs := child.Requests
	if len(reqs) != 2 || !strings.Contains(strings.Join(roleTexts(reqs[1]), "\n"), "user: find A\nassistant: A\nuser: "+wantFollowUp) {
		t.Fatalf("child requests = %+v", reqs)
	}
	if r := record(t, h.st, "async_1"); r.Status != StatusCompleted || r.Result != "B" {
		t.Fatalf("record = %+v", r)
	}
}

func TestContinueUnsupportedHandle(t *testing.T) {
	f := &fakeRunner{out: Outcome{Status: StatusCompleted, Text: "done"}}
	h := newHarness(t, Config{Runner: f, DisableStopReminder: true},
		start("s1", "general-purpose", "x"), llmtest.Turn{Text: "started"},
		update("u1", "async_1", "more"), llmtest.Turn{Text: "ok"})
	h.run(t, context.Background(), "go", nil)
	evs, _ := h.run(t, context.Background(), "more", nil)
	if res := result(t, evs, ToolUpdate); !res.IsError || !strings.Contains(res.Flatten(), "already finished") {
		t.Fatalf("update_task = %+v", res)
	}
}

func TestContinueRespectsMaxRunning(t *testing.T) {
	quick := llmtest.New(llmtest.Turn{Text: "A"}, llmtest.Turn{Text: "B"})
	slow := newGate(llmtest.Turn{Text: "slow"})
	t.Cleanup(slow.release)
	specs := []subagents.Subagent{
		{Name: "quick", Description: "Quick.", SystemPrompt: "q", Provider: quick},
		{Name: "slow", Description: "Slow.", SystemPrompt: "s", Provider: slow},
	}
	h := newHarness(t, Config{Subagents: specs, MaxRunning: 1, DisableStopReminder: true},
		start("s1", "quick", "a"), llmtest.Turn{Text: "started"},
		start("s2", "slow", "b"), update("u1", "async_1", "more"), llmtest.Turn{Text: "ok"})
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	evs, _ := h.run(t, context.Background(), "more", nil)
	if res := result(t, evs, ToolStart); res.IsError {
		t.Fatalf("start_task = %+v", res)
	}
	if res := result(t, evs, ToolUpdate); !res.IsError || !strings.Contains(res.Flatten(), "limit 1") {
		t.Fatalf("update_task = %+v", res)
	}
	if r := record(t, h.st, "async_1"); r.Status != StatusCompleted || r.Result != "A" {
		t.Fatalf("record of async_1 = %+v", r)
	}
	if len(quick.Requests) != 1 {
		t.Fatalf("the refused continue ran an episode: %d requests", len(quick.Requests))
	}
}

func TestPrunedRecordDropsFallbackThread(t *testing.T) {
	child := llmtest.New(llmtest.Turn{Text: "A"}, llmtest.Turn{Text: "B"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(child)}, MaxFinished: 1, DisableStopReminder: true},
		start("s1", "researcher", "a"), llmtest.Turn{Text: "started"},
		start("s2", "researcher", "b"), llmtest.Turn{Text: "started"},
		llmtest.Turn{Text: "noted"})
	fb := h.m.runner.(*localRunner).fallback
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	if _, _, err := fb.Load(context.Background(), "t/async_1"); err != nil {
		t.Fatalf("fallback thread of async_1: %v", err)
	}
	h.run(t, context.Background(), "more", nil) // announces async_1, starts async_2
	waitEnded(t, h.m, "async_2")
	h.run(t, context.Background(), "more", nil) // announces async_2: async_1's record is pruned
	if hasRecord(h.st, "async_1") {
		t.Fatalf("async_1 was not pruned: %+v", Tasks(h.st))
	}
	if _, _, err := fb.Load(context.Background(), "t/async_1"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("fallback thread of the pruned async_1: %v, want ErrNotFound", err)
	}
	if _, _, err := fb.Load(context.Background(), "t/async_2"); err != nil {
		t.Fatalf("fallback thread of async_2: %v", err)
	}
}

// modeSpy records the permission mode and depth of every run on a task
// thread when it calls its model.
type modeSpy struct {
	mu     sync.Mutex
	modes  []permission.Mode
	depths []int
}

func (s *modeSpy) hooks() *hooks {
	return &hooks{beforeModel: func(ctx context.Context, st *state.State) {
		info, ok := agent.Info(ctx)
		if !ok || !strings.Contains(info.ThreadID, "/async_") {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.modes = append(s.modes, info.Agent.PermissionMode(st))
		s.depths = append(s.depths, info.Depth)
	}}
}

func TestContinueUsesStricterParentMode(t *testing.T) {
	spy := &modeSpy{}
	child := llmtest.New(llmtest.Turn{Text: "A"}, llmtest.Turn{Text: "B"})
	spec := researcher(child)
	spec.PermissionMode = permission.ModeAcceptEdits
	h := newHarnessWith(t, Config{Subagents: []subagents.Subagent{spec}, DisableStopReminder: true}, []agent.Middleware{spy.hooks()},
		start("s1", "researcher", "find A"), llmtest.Turn{Text: "started"},
		update("u1", "async_1", "and B?"), llmtest.Turn{Text: "asked"})
	if err := agent.SetPermissionMode(h.st, permission.ModeAcceptEdits); err != nil {
		t.Fatal(err)
	}
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	// The parent is now in plan mode: the task may not write any more.
	if err := agent.SetPermissionMode(h.st, permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	evs, _ := h.run(t, context.Background(), "ask more", nil)
	if res := result(t, evs, ToolUpdate); res.IsError {
		t.Fatalf("update_task = %+v", res)
	}
	waitEnded(t, h.m, "async_1")
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.modes) != 2 || spy.modes[0] != permission.ModeAcceptEdits || spy.modes[1] != permission.ModePlan {
		t.Fatalf("task modes = %v, want [acceptEdits plan]", spy.modes)
	}
	// Both episodes run nested in the parent, the continued one included.
	if spy.depths[0] != 1 || spy.depths[1] != 1 {
		t.Fatalf("task depths = %v, want [1 1]", spy.depths)
	}
}

func TestContinueAfterReleaseIsNested(t *testing.T) {
	spy := &modeSpy{}
	child := llmtest.New(llmtest.Turn{Text: "A"}, llmtest.Turn{Text: "B"})
	h := newHarnessWith(t, Config{Subagents: []subagents.Subagent{researcher(child)}, DisableStopReminder: true}, []agent.Middleware{spy.hooks()},
		start("s1", "researcher", "find A"), llmtest.Turn{Text: "started"},
		llmtest.Turn{Text: "noted"}, llmtest.Turn{Text: "idle"},
		update("u1", "async_1", "and B?"), llmtest.Turn{Text: "asked"})
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	h.run(t, context.Background(), "more", nil)
	h.run(t, context.Background(), "more", nil)
	if h.m.jobs.Get("t", "async_1") != nil {
		t.Fatal("the job is still held")
	}
	h.run(t, context.Background(), "ask more", nil)
	waitEnded(t, h.m, "async_1")
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.depths) != 2 || spy.depths[1] != 1 {
		t.Fatalf("task depths = %v, want the reattached episode at 1", spy.depths)
	}
}

// contHandle is an ended ctlHandle that is a Continuer: each Continue
// returns a new running ctlHandle after a pause.
type contHandle struct {
	*ctlHandle
	calls atomic.Int32
	mu    sync.Mutex
	next  []*ctlHandle
}

func (c *contHandle) Continue(context.Context, string) (Handle, error) {
	c.calls.Add(1)
	time.Sleep(10 * time.Millisecond) // widen the window for a racing caller
	h := newCtlHandle(Outcome{Status: StatusCancelled})
	c.mu.Lock()
	c.next = append(c.next, h)
	c.mu.Unlock()
	return h, nil
}

func TestContinueClaimsTaskOnce(t *testing.T) {
	tk := newTask(context.Background())
	h := &contHandle{ctlHandle: ended(Outcome{Status: StatusCompleted, Text: "x"})}
	tk.attach(h)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { errs[i] = tk.continueWith(context.Background(), "more") })
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, errStillRunning):
			t.Fatalf("losing continue = %v, want errStillRunning", err)
		}
	}
	if wins != 1 || h.calls.Load() != 1 {
		t.Fatalf("%d continues won, Continue called %d times; want 1 and 1", wins, h.calls.Load())
	}
	if isClosed(tk.Done()) {
		t.Fatal("the task's Done is closed after a continue")
	}
	h.next[0].end(Outcome{Status: StatusCompleted, Text: "y"})
	select {
	case <-tk.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the second episode's end did not close Done")
	}
	if out, _, _ := tk.outcome(); out.Text != "y" {
		t.Fatalf("outcome = %+v", out)
	}
}

// waitingOutcome is the outcome of a task waiting to write a file.
func waitingOutcome() Outcome {
	return Outcome{Status: StatusWaiting, Pending: &agent.Interrupt{Requests: []agent.ActionRequest{{ToolUseID: "w1", Tool: "write_file"}}}}
}

func TestContinueAfterCancelledWaitingDoesNotRunPending(t *testing.T) {
	h1 := &contHandle{ctlHandle: ended(waitingOutcome())}
	run := funcRunner(func(context.Context, Request) (Handle, error) { return h1, nil })
	h := newHarness(t, Config{Runner: run, DisableStopReminder: true},
		start("s1", "general-purpose", "x"), llmtest.Turn{Text: "started"},
		// One turn, so the cancelled task is still held when it is updated.
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "c1", Name: ToolCancel, Input: obj{"task_id": "async_1"}},
			{ID: "u1", Name: ToolUpdate, Input: obj{"task_id": "async_1", "message": "go on"}},
		}},
		llmtest.Turn{Text: "ok"})
	h.run(t, context.Background(), "go", nil)
	evs, _ := h.run(t, context.Background(), "more", nil)
	if res := result(t, evs, ToolCancel); res.IsError || res.Flatten() != "Cancelled task async_1." {
		t.Fatalf("cancel_task = %+v", res)
	}
	if h1.stops.Load() != 1 {
		t.Fatalf("the waiting Handle was stopped %d times, want 1", h1.stops.Load())
	}
	if res := result(t, evs, ToolUpdate); !res.IsError || !strings.Contains(res.Flatten(), "cancelled while it waited") {
		t.Fatalf("update_task = %+v", res)
	}
	if h1.calls.Load() != 0 {
		t.Fatal("a cancelled waiting task was continued")
	}
	if r := record(t, h.st, "async_1"); r.Status != StatusCancelled || r.Pending != nil {
		t.Fatalf("record = %+v", r)
	}
}

func TestStopRacingInterruptIsCancelled(t *testing.T) {
	// The task stops waiting as it is stopped: it was cancelled.
	hd := newCtlHandle(waitingOutcome())
	run := funcRunner(func(context.Context, Request) (Handle, error) { return hd, nil })
	h := newHarness(t, Config{Runner: run, DisableStopReminder: true},
		start("s1", "general-purpose", "x"), call("c1", ToolCancel, obj{"task_id": "async_1"}), llmtest.Turn{Text: "ok"})
	evs, _ := h.run(t, context.Background(), "go", nil)
	if res := result(t, evs, ToolCancel); res.IsError || res.Flatten() != "Cancelled task async_1." {
		t.Fatalf("cancel_task = %+v", res)
	}
	if r := record(t, h.st, "async_1"); r.Status != StatusCancelled || r.Pending != nil || !r.Notified {
		t.Fatalf("record = %+v", r)
	}
}

func TestEndNestedCancelsWaitingTask(t *testing.T) {
	hd := ended(waitingOutcome())
	run := funcRunner(func(context.Context, Request) (Handle, error) { return hd, nil })
	h := newHarness(t, Config{Runner: run, DisableStopReminder: true},
		start("s1", "general-purpose", "x"), llmtest.Turn{Text: "started"})
	h.run(t, context.Background(), "go", nil)
	ctx, _ := agenttest.Context(context.Background(), agent.RunInfo{ThreadID: "t", Depth: 1})
	if err := h.m.AfterAgent(ctx, h.st); err != nil {
		t.Fatal(err)
	}
	if r := record(t, h.st, "async_1"); r.Status != StatusCancelled || !r.Notified {
		t.Fatalf("record = %+v", r)
	}
	if hd.stops.Load() != 1 {
		t.Fatalf("Stop called %d times, want 1", hd.stops.Load())
	}
}

func TestCloseStopsWaitingTask(t *testing.T) {
	hd := ended(waitingOutcome())
	run := funcRunner(func(context.Context, Request) (Handle, error) { return hd, nil })
	h := newHarness(t, Config{Runner: run, DisableStopReminder: true},
		start("s1", "general-purpose", "x"), llmtest.Turn{Text: "started"})
	h.run(t, context.Background(), "go", nil)
	if err := h.m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hd.stops.Load() != 1 {
		t.Fatalf("Stop called %d times, want 1", hd.stops.Load())
	}
	recs := h.m.updateRecords(h.st, "t", nil)
	if len(recs) != 1 || recs[0].Status != StatusCancelled {
		t.Fatalf("records after Close = %+v", recs)
	}
}

func TestWaitingRecordWithoutJobStaysWaiting(t *testing.T) {
	hd := ended(waitingOutcome())
	run := funcRunner(func(context.Context, Request) (Handle, error) { return hd, nil })
	h := newHarness(t, Config{Runner: run, DisableStopReminder: true},
		start("s1", "general-purpose", "x"), llmtest.Turn{Text: "started"})
	h.run(t, context.Background(), "go", nil)
	other, err := New(Config{Runner: run})
	if err != nil {
		t.Fatal(err)
	}
	recs := other.updateRecords(h.st, "t", nil)
	if len(recs) != 1 || recs[0].Status != StatusWaiting || recs[0].Pending == nil {
		t.Fatalf("records seen by a middleware without the job = %+v", recs)
	}
}

func TestRecordedPendingIsCapped(t *testing.T) {
	big := `"` + strings.Repeat("x", 2*MaxRecordedResult) + `"`
	p := &agent.Interrupt{Requests: []agent.ActionRequest{{ToolUseID: "w1", Tool: "write_file", Input: []byte(big), Reason: strings.Repeat("r", 2*MaxRecordedResult)}}}
	got := recordedPending(p)
	req := got.Requests[0]
	if len(req.Reason) > MaxRecordedResult || len(req.Input) > MaxRecordedResult+64 {
		t.Fatalf("recorded request: reason %d bytes, input %d bytes", len(req.Reason), len(req.Input))
	}
	s := state.New()
	tasksKey.Set(s, []TaskRecord{{ID: "async_1", ThreadID: "t", Status: StatusWaiting, Pending: got}})
	if _, err := s.MarshalJSON(); err != nil {
		t.Fatalf("a capped record does not encode: %v", err)
	}
	if len(p.Requests[0].Input) != len(big) {
		t.Fatal("recordedPending changed its argument")
	}
}
