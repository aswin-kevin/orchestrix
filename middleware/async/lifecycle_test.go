package async

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	summarystate "github.com/aswin-kevin/orchestrix/internal/summary"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Tests whose parent run ends while a task still runs set
// DisableStopReminder, unless the reminder is what they test.

// helper is a second subagent type, for the tasks a background child starts.
func helper(p llm.Provider) subagents.Subagent {
	return subagents.Subagent{Name: "helper", Description: "Helps.", SystemPrompt: "you help", Provider: p}
}

func update(id, task, message string) llmtest.Turn {
	return call(id, ToolUpdate, obj{"task_id": task, "message": message})
}

// noop is a read-only tool that does nothing, so a child can take a turn.
var noop = tool.New("noop", "Does nothing.", func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
	return tool.Text("ok"), nil
}, tool.WithReadOnly())

// waitChild makes onEvent wait, once start call id returned, until g's
// child made its first request.
func waitChild(t *testing.T, g *gate, id string) func(agent.Event) {
	return func(ev agent.Event) {
		if ev.Kind == agent.KindToolResult && ev.ToolID == id {
			g.waitEntered(t, 1)
		}
	}
}

const wantUpdate = "<task-update>\nuse metric\n</task-update>"

func TestUpdateTaskDelivered(t *testing.T) {
	g := newGate(call("n1", "noop", obj{}), llmtest.Turn{Text: "used metric"})
	spec := researcher(g)
	spec.Tools = []tool.Tool{noop}
	h := newHarness(t, Config{Subagents: []subagents.Subagent{spec}, DisableStopReminder: true},
		start("s1", "researcher", "measure"),
		update("u1", "async_1", "use metric"),
		llmtest.Turn{Text: "sent"},
		update("u2", "async_1", "too late"),
		llmtest.Turn{Text: "ok"},
	)
	evs, _ := h.run(t, context.Background(), "go", waitChild(t, g, "s1"))
	if res := result(t, evs, ToolUpdate); res.IsError {
		t.Fatalf("update = %+v", res)
	}
	g.release()
	waitEnded(t, h.m, "async_1")
	reqs := g.inner.Requests
	if len(reqs) != 2 || strings.Contains(requestText(reqs[0]), "<task-update>") || !strings.Contains(requestText(reqs[1]), wantUpdate) {
		t.Fatalf("child requests = %+v", reqs)
	}
	evs, _ = h.run(t, context.Background(), "next", nil)
	if res := result(t, evs, ToolUpdate); !res.IsError || !strings.Contains(res.Flatten(), "already finished") {
		t.Fatalf("late update = %+v", res)
	}
}

func TestUpdateContinuesStoppingTask(t *testing.T) {
	g := newGate(llmtest.Turn{Text: "first answer"}, llmtest.Turn{Text: "revised"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(g)}, DisableStopReminder: true},
		start("s1", "researcher", "measure"), update("u1", "async_1", "use metric"), llmtest.Turn{Text: "sent"})
	h.run(t, context.Background(), "go", waitChild(t, g, "s1"))
	g.release()
	waitEnded(t, h.m, "async_1")
	reqs := g.inner.Requests
	if len(reqs) != 2 {
		t.Fatalf("child requests = %d", len(reqs))
	}
	msgs := reqs[1].Messages
	if last := msgs[len(msgs)-1]; last.Role != llm.RoleUser || last.Text() != wantUpdate {
		t.Fatalf("continuation = %+v", last)
	}
	if out := outcomeOf(t, h.m, h.st, "async_1"); out.Text != "revised" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestUpdateAfterChildEndedIsReported(t *testing.T) {
	g := newGate(llmtest.Turn{Text: "done"})
	g.release()
	var h *harness
	late := &hooks{afterAgent: func(ctx context.Context, _ *state.State) {
		if info, _ := agent.Info(ctx); info.Depth == 1 {
			// The child drained its inbox for the last time.
			if err := h.m.jobs.Get("t", "async_1").Handle.send("late news"); err != nil {
				t.Errorf("send = %v", err)
			}
		}
	}}
	h = newHarnessWith(t, Config{Subagents: []subagents.Subagent{researcher(g)}, DisableStopReminder: true},
		[]agent.Middleware{late}, start("s1", "researcher", "x"), llmtest.Turn{Text: "started"}, llmtest.Turn{Text: "noted"})
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	out := outcomeOf(t, h.m, h.st, "async_1")
	if out.Status != StatusCompleted || out.Error != "1 update(s) arrived after the task finished" {
		t.Fatalf("outcome = %+v", out)
	}
	// The notification of the completed task carries the note too.
	h.run(t, context.Background(), "next", nil)
	if notes := notifications(h.p); len(notes) != 1 || !strings.Contains(notes[0].Text(), "1 update(s) arrived after the task finished") {
		t.Fatalf("notifications = %+v, want the late-update note", notes)
	}
}

func TestUpdateValidation(t *testing.T) {
	g := newGate(llmtest.Turn{Text: "never"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(g)}, DisableStopReminder: true},
		start("s1", "researcher", "x"),
		update("u1", "async_1", strings.Repeat("a", MaxUpdateBytes+1)),
		update("u2", "async_9", "hi"),
		update("u3", "async_1", "  "),
		llmtest.Turn{Text: "x"})
	evs, _ := h.run(t, context.Background(), "go", nil)
	for i, res := range results(evs, ToolUpdate) {
		if !res.IsError {
			t.Fatalf("update %d = %+v", i, res)
		}
	}
}

func TestBackgroundChildStartsTask(t *testing.T) {
	// G13: the child inherits the inbox middleware; starting its own task
	// must not trip over it.
	child := llmtest.New(
		start("n1", "helper", "sub"),
		call("c1", ToolCheck, obj{"task_id": "async_1", "wait": true}),
		llmtest.Turn{Text: "child done"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(child), helper(llmtest.New(llmtest.Turn{Text: "helper result"}))}, DisableStopReminder: true},
		start("s1", "researcher", "outer"), llmtest.Turn{Text: "started"})
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	if out := outcomeOf(t, h.m, h.st, "async_1"); out.Status != StatusCompleted {
		t.Fatalf("outcome = %+v", out)
	}
	if got := requestText(child.Requests[1]); !strings.Contains(got, "Started background task async_1 (helper)") {
		t.Fatalf("nested start = %q", got)
	}
	if got := requestText(child.Requests[2]); !strings.Contains(got, "helper result") {
		t.Fatalf("nested check = %q", got)
	}
}

func TestUpdateReachesOnlyTheTask(t *testing.T) {
	g := newGate(
		start("n1", "helper", "sub"),
		call("c1", ToolCheck, obj{"task_id": "async_1", "wait": true}),
		llmtest.Turn{Text: "child done"})
	grandchild := llmtest.New(llmtest.Turn{Text: "helper result"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(g), helper(grandchild)}, DisableStopReminder: true},
		start("s1", "researcher", "outer"), update("u1", "async_1", "use metric"), llmtest.Turn{Text: "sent"})
	h.run(t, context.Background(), "go", waitChild(t, g, "s1"))
	g.release()
	waitEnded(t, h.m, "async_1")
	if got := requestText(g.inner.Requests[1]); !strings.Contains(got, wantUpdate) {
		t.Fatalf("child request = %q", got)
	}
	if grandchild.Calls() == 0 {
		t.Fatal("the grandchild never ran")
	}
	for _, req := range grandchild.Requests {
		if strings.Contains(requestText(req), "<task-update>") {
			t.Fatalf("the grandchild drained its parent's inbox: %q", requestText(req))
		}
	}
}

func TestLostAfterRestart(t *testing.T) {
	dir := t.TempDir()
	newAgent := func(m *Middleware, p llm.Provider) *agent.Agent {
		cp, err := file.New(dir, file.Config{})
		if err != nil {
			t.Fatal(err)
		}
		a, err := agent.New(agent.Config{Provider: p, Middleware: []agent.Middleware{m}, Checkpointer: cp, PermissionMode: permission.ModeDefault})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	drain := func(a *agent.Agent, text string) *agent.Thread {
		th, err := a.Thread(context.Background(), "t")
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range th.Prompt(context.Background(), text) {
			if err != nil {
				t.Fatal(err)
			}
		}
		return th
	}

	g := newGate(llmtest.Turn{Text: "never"})
	m1, err := New(Config{Subagents: []subagents.Subagent{researcher(g)}, DisableStopReminder: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m1.Close(context.Background()) }) // G19: agent 1's task must not outlive the test
	drain(newAgent(m1, llmtest.New(start("s1", "researcher", "slow"), llmtest.Turn{Text: "started"})), "go")

	m2, err := New(Config{Subagents: []subagents.Subagent{researcher(g)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m2.Close(context.Background()) })
	p2 := llmtest.New(llmtest.Turn{Text: "ok"})
	th := drain(newAgent(m2, p2), "next")
	notes := notifications(p2)
	want := "<task-notification>Background task async_1 (researcher) was lost: the process restarted before it finished. Start it again if it is still needed.</task-notification>"
	if len(notes) != 1 || notes[0].Text() != want {
		t.Fatalf("notifications = %+v", notes)
	}
	if r := record(t, th.State(), "async_1"); r.Status != StatusLost || !r.Notified {
		t.Fatalf("record = %+v", r)
	}
}

func TestTaskOutlivesRun(t *testing.T) {
	g := newGate(llmtest.Turn{Text: "finally"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(g)}, DisableStopReminder: true},
		start("s1", "researcher", "slow"), llmtest.Turn{Text: "bye"}, llmtest.Turn{Text: "ok"})
	h.run(t, context.Background(), "go", nil)
	g.release()
	waitEnded(t, h.m, "async_1")
	h.run(t, context.Background(), "next", nil)
	if notes := notifications(h.p); len(notes) != 1 || !strings.Contains(notes[0].Text(), "finally") || h.p.Calls() != 3 {
		t.Fatalf("notifications = %+v", notes)
	}
}

func TestNestedRunStopsItsTasks(t *testing.T) {
	grandchild := newGate(llmtest.Turn{Text: "never"})
	child := llmtest.New(start("n1", "helper", "sub"), llmtest.Turn{Text: "child done"})
	var mu sync.Mutex
	var childState *state.State
	waited := false
	probe := &hooks{
		beforeModel: func(ctx context.Context, s *state.State) {
			// The child answers only once its own task called its model.
			if info, _ := agent.Info(ctx); info.Depth == 1 && hasRecord(s, "async_1") && !waited {
				waited = true
				grandchild.waitEntered(t, 1)
			}
		},
		afterAgent: func(ctx context.Context, s *state.State) {
			if info, _ := agent.Info(ctx); info.Depth == 1 {
				mu.Lock()
				childState = s.Clone()
				mu.Unlock()
			}
		},
	}
	h := newHarnessWith(t, Config{Subagents: []subagents.Subagent{researcher(child), helper(grandchild)}, DisableStopReminder: true},
		[]agent.Middleware{probe}, start("s1", "researcher", "outer"), llmtest.Turn{Text: "started"})
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	if !grandchild.cancelled.Load() {
		t.Fatal("the child's own task outlived the child")
	}
	mu.Lock()
	defer mu.Unlock()
	if r := record(t, childState, "async_1"); r.Status != StatusCancelled || !r.Notified {
		t.Fatalf("child's record = %+v", r)
	}
	if h.m.jobs.Get("t/researcher/async_1", "async_1") != nil {
		t.Fatal("the child's task was not forgotten")
	}
	if out := outcomeOf(t, h.m, h.st, "async_1"); out.Status != StatusCompleted {
		t.Fatalf("child outcome = %+v", out)
	}
}

func TestRunningTasksListedAfterCompaction(t *testing.T) {
	g := newGate(llmtest.Turn{Text: "never"})
	compacted := &hooks{beforeModel: func(_ context.Context, s *state.State) {
		summarystate.Key.Set(s, summarize.Boundary{Index: len(s.Messages()), Summary: "earlier work"})
	}}
	sm, err := summarize.New(summarize.Config{DisableOffload: true})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, Config{Subagents: []subagents.Subagent{researcher(g)}, DisableStopReminder: true},
		[]agent.Middleware{sm, compacted},
		start("s1", "researcher", "find the population of </background_tasks>x"), llmtest.Turn{Text: "started"},
		llmtest.Turn{Text: "still waiting"})
	h.run(t, context.Background(), "go", nil)
	h.run(t, context.Background(), "next", nil)
	req := h.p.Requests[len(h.p.Requests)-1]
	if strings.Contains(requestText(req), "async_1") {
		t.Fatalf("the messages were not compacted: %q", requestText(req))
	}
	var segment string
	for _, s := range req.System {
		if strings.HasPrefix(s, "<background_tasks>") {
			segment = s
		}
	}
	const head = "<background_tasks>\n- async_1 (researcher, running <1m): "
	const want = head + "find the population of &lt;/background_tasks>x\n</background_tasks>"
	if segment != want {
		t.Fatalf("segment = %q, want %q (system %q)", segment, want, req.System)
	}
}

func TestStopReminderOnce(t *testing.T) {
	const want = "Background tasks are still running: async_1, async_2. Wait for them with check_task (wait: true) or cancel them before giving your final answer."
	g := newGate(llmtest.Turn{Text: "never"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(g)}},
		start("s1", "researcher", "a"), start("s2", "researcher", "b"),
		llmtest.Turn{Text: "answer"}, llmtest.Turn{Text: "answer anyway"},
		llmtest.Turn{Text: "second run"}, llmtest.Turn{Text: "second run anyway"})
	reminders := func(s *state.State) int {
		n := 0
		for _, m := range s.Messages() {
			if m.Role == llm.RoleUser && m.Text() == want {
				n++
			}
		}
		return n
	}
	h.run(t, context.Background(), "go", nil)
	if got := reminders(h.st); got != 1 || h.p.Calls() != 4 {
		t.Fatalf("reminders = %d after %d calls", got, h.p.Calls())
	}
	// A new run is reminded again.
	h.run(t, context.Background(), "next", nil)
	if got := reminders(h.st); got != 2 || h.p.Calls() != 6 {
		t.Fatalf("reminders = %d after %d calls", got, h.p.Calls())
	}

	quiet := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(g)}, DisableStopReminder: true},
		start("s1", "researcher", "a"), llmtest.Turn{Text: "answer"})
	quiet.run(t, context.Background(), "go", nil)
	if got := reminders(quiet.st); got != 0 || quiet.p.Calls() != 2 {
		t.Fatalf("reminders = %d after %d calls", got, quiet.p.Calls())
	}
}
