package async

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/state"
)

// childThread is the thread of the background child started as async_1 of
// thread "t": its Request.TaskThreadID.
const childThread = "t/async_1"

func TestTopLevelThreadReleasedAfterRun(t *testing.T) {
	var m *Middleware // set before the run starts
	probe := &hooks{beforeModel: func(_ context.Context, s *state.State) {
		// The last turn starts once the task ended, so it announces it.
		if hasRecord(s, "async_1") {
			waitJobEnded(t, func() bool {
				j := m.jobs.Get("t", "async_1")
				return j == nil || j.Ended()
			})
		}
	}}
	h := newHarnessWith(t, Config{Runner: &fakeRunner{out: Outcome{Status: StatusCompleted, Text: "done"}}},
		[]agent.Middleware{probe}, start("s1", subagents.GeneralPurpose, "quick"), llmtest.Turn{Text: "answer"})
	m = h.m

	h.run(t, context.Background(), "go", nil)

	if r := record(t, h.st, "async_1"); !r.terminal() || !r.Notified {
		t.Fatalf("record = %+v, want an ended task the model was told about", r)
	}
	if n := h.m.jobs.ThreadCount(); n != 0 {
		t.Fatalf("%d thread entries after the run, want 0: every task ended and was announced", n)
	}
}

func TestNestedThreadReleasedAfterRun(t *testing.T) {
	grandchild := llmtest.New(llmtest.Turn{Text: "sub result"})
	child := llmtest.New(start("n1", "helper", "sub"), llmtest.Turn{Text: "child done"})
	var m *Middleware // set before the run starts
	probe := &hooks{beforeModel: func(ctx context.Context, s *state.State) {
		// The child's last turn starts only once its own task ended, so
		// that turn announces it.
		if info, _ := agent.Info(ctx); info.Depth == 1 && hasRecord(s, "async_1") {
			waitJobEnded(t, func() bool {
				j := m.jobs.Get(info.ThreadID, "async_1")
				return j == nil || j.Ended()
			})
		}
	}}
	h := newHarnessWith(t, Config{Subagents: []subagents.Subagent{researcher(child), helper(grandchild)}, DisableStopReminder: true},
		[]agent.Middleware{probe}, start("s1", "researcher", "outer"), llmtest.Turn{Text: "started"})
	m = h.m

	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")

	if grandchild.Calls() != 1 || child.Calls() != 2 {
		t.Fatalf("grandchild calls %d, child calls %d; want 1 and 2", grandchild.Calls(), child.Calls())
	}
	if h.m.jobs.Get(childThread, "async_1") != nil {
		t.Fatal("the child's ended task is still held after the child's run")
	}
	// Only the parent's thread, whose task has not been announced, is left.
	if n := h.m.jobs.ThreadCount(); n != 1 {
		t.Fatalf("%d thread entries, want 1 (the parent's)", n)
	}
}

// waitJobEnded polls done for up to five seconds.
func waitJobEnded(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Error("the child's task did not end")
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCheckTaskCapsResult(t *testing.T) {
	huge := strings.Repeat("r", 3*MaxCheckResultBytes)
	runner := funcRunner(func(context.Context, Request) (Handle, error) {
		return ended(Outcome{Status: StatusCompleted, Text: huge}), nil
	})
	h := newHarness(t, Config{Runner: runner},
		start("s1", subagents.GeneralPurpose, "big"),
		call("c1", ToolCheck, obj{"task_id": "async_1", "wait": true}),
		llmtest.Turn{Text: "done"})

	evs, _ := h.run(t, context.Background(), "go", nil)

	res := result(t, evs, ToolCheck).Flatten()
	if len(res) > MaxCheckResultBytes+500 || !strings.Contains(res, "truncated") {
		t.Fatalf("check_task result is %d bytes (truncation noted: %v), want at most about %d",
			len(res), strings.Contains(res, "truncated"), MaxCheckResultBytes)
	}
}
