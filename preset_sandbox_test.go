//go:build unix

package orchestrix_test

import (
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/sandbox"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
	"github.com/aswin-kevin/orchestrix/permission"
)

func TestWithTasksRejectsSandboxWithoutStarter(t *testing.T) {
	sb, err := sandbox.New(shellBackend(t).(*shell.Backend), sandbox.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tm := tasks.New(tasks.Config{})
	defer tm.Shutdown()
	if _, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithBackend(sb), orchestrix.WithTasks(tm)); err == nil {
		t.Fatal("WithTasks accepted a sandbox that cannot start commands")
	}
}

func TestWithTasksRunsJobInSandbox(t *testing.T) {
	dir := t.TempDir()
	sh, err := shell.New(dir, shell.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := sandbox.NewStarter(sh, sandbox.Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	tm := tasks.New(tasks.Config{})
	defer tm.Shutdown()
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e1", Name: "execute", Input: map[string]any{
			"command": "pwd; echo job-done", "run_in_background": true,
		}}}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "o1", Name: tasks.ToolTaskOutput, Input: map[string]any{
			"task_id": "bg_1", "block": true, "timeout_seconds": 10,
		}}}},
		llmtest.Turn{Text: "ok"},
	)
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithBackend(sb), orchestrix.WithTasks(tm),
		orchestrix.WithPermissionMode(permission.ModeBypass))
	if err != nil {
		t.Fatal(err)
	}
	var final bool
	for ev := range a.Run(t.Context(), agent.Input{ThreadID: "t1", Messages: []llm.Message{llm.UserText("go")}}) {
		if ev.Final != nil {
			if ev.Final.Err != nil {
				t.Fatal(ev.Final.Err)
			}
			final = true
		}
	}
	if !final {
		t.Fatal("no final event")
	}
	out := scriptedResult(p, "o1")
	if !strings.Contains(out, "job-done") || !strings.Contains(out, dir) {
		t.Fatalf("task_output = %q, want the job's output from %s", out, dir)
	}
}

// scriptedResult returns the text of the tool result answering call id.
func scriptedResult(p *llmtest.Scripted, id string) string {
	var parts []string
	for _, req := range p.Requests {
		for _, msg := range req.Messages {
			for _, b := range msg.Content {
				if b.Type == llm.BlockToolResult && b.ToolUseID == id {
					for _, c := range b.Content {
						parts = append(parts, c.Text)
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}
