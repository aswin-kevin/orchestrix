package subagents

import (
	"context"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
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

// restoredModeKey declares the agent's permission-mode key again (same name,
// type and privacy), so a test can store a mode SetPermissionMode would
// refuse, as a restored checkpoint might hold.
var restoredModeKey = state.NewKey[permission.Mode]("agent.permission_mode", state.Private[permission.Mode]())

// mustSetMode sets the run's permission mode in s or fails the test.
func mustSetMode(t *testing.T, s *state.State, m permission.Mode) {
	t.Helper()
	if err := agent.SetPermissionMode(s, m); err != nil {
		t.Fatal(err)
	}
}

// modeRun is one task call to a "writer" subagent whose only turn calls
// write_file.
type modeRun struct {
	// parentMode is the parent's Config.PermissionMode; runMode, when
	// non-empty, is stored as the run's permission mode on the parent's
	// State before the run.
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
	be := testutil.Memory(t)
	a, err := agent.New(agent.Config{
		Provider: parent, Backend: be, Middleware: []agent.Middleware{mw}, PermissionMode: r.parentMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := userInput("go")
	in.State = state.New()
	if r.runMode != "" {
		// Set directly, as a restored checkpoint would, so an invalid mode
		// can be tested.
		restoredModeKey.Set(in.State, r.runMode)
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
	t.Run("parent not in plan", func(t *testing.T) {
		// Bypass rather than default: a child never runs more freely than
		// its parent, so under a default parent it would ask.
		got := modeRun{parentMode: permission.ModeBypass, childMode: permission.ModeBypass}.run(t)
		if res := got.childWriteResult(t); res.IsError {
			t.Fatalf("child write_file result = %q, want success", blockText(res))
		}
		if !got.written {
			t.Fatal("the child did not write /out.md")
		}
	})
}

func TestChildModeIsStricter(t *testing.T) {
	const dontAskDenied = "Permission denied: write_file is not permitted in dontAsk mode"
	for _, tc := range []struct {
		name string
		run  modeRun
		// denied is the child's write_file error, "" when it must write.
		denied string
	}{
		{"dontAsk child under acceptEdits run", modeRun{parentMode: permission.ModeBypass, runMode: permission.ModeAcceptEdits, childMode: permission.ModeDontAsk}, dontAskDenied},
		{"bypass child under dontAsk parent", modeRun{parentMode: permission.ModeDontAsk, childMode: permission.ModeBypass}, dontAskDenied},
		{"bypass child under dontAsk run", modeRun{parentMode: permission.ModeBypass, runMode: permission.ModeDontAsk, childMode: permission.ModeBypass}, dontAskDenied},
		{"bypass child under plan parent config", modeRun{parentMode: permission.ModePlan, childMode: permission.ModeBypass}, planDenied},
		{"bypass child under invalid restored mode", modeRun{parentMode: permission.ModeBypass, runMode: "nope", childMode: permission.ModeBypass}, planDenied},
		{"acceptEdits child under bypass parent", modeRun{parentMode: permission.ModeBypass, childMode: permission.ModeAcceptEdits}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.run.run(t)
			res := got.childWriteResult(t)
			if tc.denied == "" {
				if res.IsError || !got.written {
					t.Fatalf("child write_file result = %q (written %v), want success", blockText(res), got.written)
				}
				return
			}
			if !res.IsError || blockText(res) != tc.denied {
				t.Fatalf("child write_file result = %q (error %v), want %q", blockText(res), res.IsError, tc.denied)
			}
			if got.written {
				t.Fatal("the child wrote /out.md")
			}
		})
	}
}

func TestGrandchildOfPlanParentIsPlan(t *testing.T) {
	// The researcher has no Middleware of its own, so it inherits the
	// parent's stack, task tool included, and delegates once more.
	child := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("inner", "researcher", "write it")}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w", Name: "write_file", Input: obj{}}}},
		llmtest.Turn{Text: "grandchild done"},
		llmtest.Turn{Text: "child done"},
	)
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "researcher", "go")}},
		llmtest.Turn{Text: "done"},
	)
	mw := newMiddleware(t, Config{Subagents: []Subagent{{
		Name: "researcher", Description: "Researches.", SystemPrompt: "you research", Provider: child,
		Tools: []tool.Tool{editTool()}, PermissionMode: permission.ModeBypass,
	}}})
	be := testutil.Memory(t)
	a, err := agent.New(agent.Config{
		Provider: parent, Backend: be, Middleware: []agent.Middleware{mw}, PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := userInput("go")
	in.State = state.New()
	mustSetMode(t, in.State, permission.ModePlan)
	_, final := collect(t, context.Background(), a, in)

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	results := toolResults(child.Requests[2])
	if len(results) != 1 || !results[0].IsError || blockText(results[0]) != planDenied {
		t.Fatalf("grandchild write_file results = %+v, want one %q", results, planDenied)
	}
	if _, err := be.Read(context.Background(), "/out.md", 0, 0); err == nil {
		t.Fatal("the grandchild wrote /out.md in plan mode")
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
	be := testutil.Memory(t)
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
