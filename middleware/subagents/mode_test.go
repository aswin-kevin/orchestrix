package subagents

import (
	"context"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

const planDenied = "Permission denied: plan mode: read-only"

// editTool writes /out.md; it is an edit, so acceptEdits mode allows it.
func editTool() tool.Tool {
	return tool.New("write_file", "Writes.", func(ctx context.Context, _ writeInput, c *tool.Call) (tool.Result, error) {
		if err := c.Backend.Write(ctx, "/out.md", []byte("report")); err != nil {
			return tool.Result{}, err
		}
		return tool.Text("written"), nil
	}, tool.WithEdit())
}

// modeRun is one task call to a "writer" subagent whose only turn calls
// write_file.
type modeRun struct {
	// parentMode is the parent's Config.PermissionMode; runMode, when
	// non-empty, is set on the parent's State before the run.
	parentMode, runMode permission.Mode
	// childMode is the writer's Subagent.PermissionMode.
	childMode permission.Mode
}

// modeOutcome is what a modeRun observed.
type modeOutcome struct {
	task    llm.Block
	child   *llmtest.Scripted
	written bool
}

func (r modeRun) run(t *testing.T) modeOutcome {
	t.Helper()
	child := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w", Name: "write_file", Input: obj{}}}},
		llmtest.Turn{Text: "child done"},
	)
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "writer", "write the report")}},
		llmtest.Turn{Text: "done"},
	)
	mw := newMiddleware(t, Config{Subagents: []Subagent{{
		Name: "writer", Description: "Writes.", SystemPrompt: "you write", Provider: child,
		Tools: []tool.Tool{editTool()}, PermissionMode: r.childMode,
	}}})
	be := memory.New()
	a, err := agent.New(agent.Config{
		Provider: parent, Backend: be, Middleware: []agent.Middleware{mw}, PermissionMode: r.parentMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := userInput("go")
	in.State = state.New()
	if err := agent.SetPermissionMode(in.State, r.runMode); err != nil {
		t.Fatal(err)
	}
	_, final := collect(t, context.Background(), a, in)
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	_, readErr := be.Read(context.Background(), "/out.md", 0, 0)
	return modeOutcome{task: onlyResult(t, parent), child: child, written: readErr == nil}
}

// childWriteResult is the result the child got for its write_file call.
func (o modeOutcome) childWriteResult(t *testing.T) llm.Block {
	t.Helper()
	if len(o.child.Requests) < 2 {
		t.Fatalf("child made %d requests, want 2", len(o.child.Requests))
	}
	results := toolResults(o.child.Requests[1])
	if len(results) != 1 {
		t.Fatalf("child got %d tool results, want 1", len(results))
	}
	return results[0]
}

func TestChildInheritsPlanMode(t *testing.T) {
	t.Run("parent in plan", func(t *testing.T) {
		got := modeRun{runMode: permission.ModePlan, childMode: permission.ModeBypass}.run(t)
		if res := got.childWriteResult(t); !res.IsError || blockText(res) != planDenied {
			t.Fatalf("child write_file result = %q (error %v), want %q", blockText(res), res.IsError, planDenied)
		}
		if got.written {
			t.Fatal("the child wrote /out.md in plan mode")
		}
	})
	t.Run("parent in default", func(t *testing.T) {
		got := modeRun{parentMode: permission.ModeDefault, childMode: permission.ModeBypass}.run(t)
		if res := got.childWriteResult(t); res.IsError {
			t.Fatalf("child write_file result = %q, want success", blockText(res))
		}
		if !got.written {
			t.Fatal("the child did not write /out.md")
		}
	})
}

func TestChildStartsInParentRunMode(t *testing.T) {
	// The parent's mode was changed during its run, so the child runs in
	// acceptEdits rather than its configured dontAsk, which would deny the
	// edit.
	got := modeRun{parentMode: permission.ModeBypass, runMode: permission.ModeAcceptEdits, childMode: permission.ModeDontAsk}.run(t)
	if res := got.childWriteResult(t); res.IsError {
		t.Fatalf("child write_file result = %q, want success in acceptEdits mode", blockText(res))
	}
	if !got.written {
		t.Fatal("the child did not write /out.md")
	}
}

func TestTaskAllowedInPlanMode(t *testing.T) {
	got := modeRun{runMode: permission.ModePlan}.run(t)
	if got.task.IsError {
		t.Fatalf("task result = %q, want the task call allowed in plan mode", blockText(got.task))
	}
}

func TestForkKeepsParentRunMode(t *testing.T) {
	// The fork is the parent's own agent on the parent's provider, so the
	// script interleaves parent and fork turns.
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", Fork, "write the report")}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w", Name: "write_file", Input: obj{}}}},
		llmtest.Turn{Text: "fork done"},
		llmtest.Turn{Text: "done"},
	)
	be := memory.New()
	a, err := agent.New(agent.Config{
		Provider: p, Backend: be, Tools: []tool.Tool{editTool()},
		Middleware: []agent.Middleware{newMiddleware(t, Config{})}, PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := userInput("go")
	in.State = state.New()
	if err := agent.SetPermissionMode(in.State, permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	_, final := collect(t, context.Background(), a, in)

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	results := toolResults(p.Requests[2])
	if len(results) != 1 || !results[0].IsError || blockText(results[0]) != planDenied {
		t.Fatalf("fork write_file results = %+v, want one %q", results, planDenied)
	}
	if _, err := be.Read(context.Background(), "/out.md", 0, 0); err == nil {
		t.Fatal("the fork wrote /out.md in plan mode")
	}
}
