//go:build unix

package tasks

import (
	"context"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/permission"
)

// newForkingAgent is newAgent with the subagents middleware, so the model
// can fork the run.
func newForkingAgent(t *testing.T, p llm.Provider, m *Middleware) *agent.Agent {
	t.Helper()
	sub, err := subagents.New(subagents.Config{})
	if err != nil {
		t.Fatalf("subagents.New: %v", err)
	}
	a, err := agent.New(agent.Config{
		Provider:       p,
		Backend:        newShell(t),
		Middleware:     []agent.Middleware{mustFS(t, filesystem.Config{Execute: true, Background: true}), m, sub},
		PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func forkCall(id string) llmtest.Call {
	return call(id, subagents.ToolTask, obj{"description": "look around", "subagent_type": subagents.Fork})
}

func TestForkLeavesParentJobRunning(t *testing.T) {
	m := newTasks(t, Config{})
	// The fork runs during the parent's third turn, so it takes the third
	// scripted response.
	p := llmtest.New(
		turn(bgExecute("e1", pidCommand)),
		turn(pidMonitor("m1", "bg_1")),
		turn(forkCall("f1")),
		llmtest.Turn{Text: "fork done"},
		turn(call("l1", ToolTaskList, obj{})),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newForkingAgent(t, p, m), input("parent"))

	if got := resultText(t, p, "l1"); !strings.Contains(got, "bg_1") || !strings.Contains(got, StatusRunning) {
		t.Fatalf("parent task_list after the fork = %q, want bg_1 running", got)
	}
	pid := pidFrom(t, p, "m1")
	if !alive(pid) {
		t.Fatalf("the fork stopped the parent's job %d", pid)
	}
	m.Shutdown()
	waitGone(t, pid)
}

func TestForkDoesNotTakeParentNotification(t *testing.T) {
	m := newTasks(t, Config{})
	// bg_1 ends while the parent's record of it still says running: sleep
	// wakes on the end without refreshing records, and the foreground
	// execute keeps the fork from running alongside the sleep.
	p := llmtest.New(
		turn(
			bgExecute("e1", "sleep 0.3; echo hi"),
			call("s1", ToolSleep, obj{"seconds": 5}),
			call("e2", filesystem.ToolExecute, obj{"command": "true"}),
			forkCall("f1"),
		),
		llmtest.Turn{Text: "fork done"},
		turn(call("l1", ToolTaskList, obj{})),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newForkingAgent(t, p, m), input("parent"))

	fork, parent := p.Requests[1], p.Requests[2]
	if got := notifications(fork); len(got) != 0 {
		t.Fatalf("the fork was told about the parent's job: %q", got)
	}
	if got := notifications(parent); len(got) != 1 || !strings.Contains(got[0], "bg_1") || !strings.Contains(got[0], StatusCompleted) {
		t.Fatalf("parent notifications after the fork = %q, want bg_1 completed", got)
	}
	if got := resultText(t, p, "l1"); !strings.Contains(got, StatusCompleted) {
		t.Fatalf("parent task_list = %q, want bg_1 completed (not lost)", got)
	}
}
