package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

const planDenied = "Permission denied: plan mode: read-only"

type setModeInput struct{}

// setModeTool switches the run to plan mode. It is read-only, so plan mode
// allows it, and not concurrency-safe, so the calls after it in the same
// turn are admitted once it has run.
func setModeTool() tool.Tool {
	return tool.New("set_mode", "Enter plan mode.",
		func(_ context.Context, _ setModeInput, c *tool.Call) (tool.Result, error) {
			if err := SetPermissionMode(c.State, permission.ModePlan); err != nil {
				return tool.Result{}, err
			}
			return tool.Text("plan mode"), nil
		},
		tool.WithReadOnly())
}

func setModeCall(id string) llmtest.Call {
	return llmtest.Call{ID: id, Name: "set_mode", Input: map[string]any{}}
}

// toolResult returns the text and error flag of the tool_result for id in s.
func toolResult(t *testing.T, s *state.State, id string) (string, bool) {
	t.Helper()
	for _, m := range s.Messages() {
		for _, b := range m.Content {
			if b.ToolUseID == id && b.Content != nil {
				return resultText(b), b.IsError
			}
		}
	}
	t.Fatalf("no tool_result for %s", id)
	return "", false
}

func TestSetPermissionModeTakesEffectSameTurn(t *testing.T) {
	log := newCallLog()
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{setModeCall("m"), writeCall("w")}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{
		Provider: p, Tools: []tool.Tool{setModeTool(), writeTool(log)},
		PermissionMode: permission.ModeBypass,
	})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	if text, isErr := toolResult(t, final.State, "w"); !isErr || text != planDenied {
		t.Fatalf("write_file result = %q (error %v), want %q", text, isErr, planDenied)
	}
	if log.count("w") != 0 {
		t.Fatal("write_file ran in plan mode")
	}
	if got := a.PermissionMode(final.State); got != permission.ModePlan {
		t.Fatalf("PermissionMode = %q, want plan", got)
	}
}

func TestSetPermissionModeInvalid(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New(), PermissionMode: permission.ModeAcceptEdits})
	s := state.New()

	if err := SetPermissionMode(s, "nope"); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("SetPermissionMode(nope) = %v, want ErrInvalidMode", err)
	}
	if got := a.PermissionMode(s); got != permission.ModeAcceptEdits {
		t.Fatalf("after an invalid mode PermissionMode = %q, want the config's", got)
	}
	if err := SetPermissionMode(s, permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	if got := a.PermissionMode(s); got != permission.ModePlan {
		t.Fatalf("PermissionMode = %q, want plan", got)
	}
	if err := SetPermissionMode(s, ""); err != nil {
		t.Fatal(err)
	}
	if got := a.PermissionMode(s); got != permission.ModeAcceptEdits {
		t.Fatalf("after clearing PermissionMode = %q, want the config's", got)
	}
	if got := a.PermissionMode(nil); got != permission.ModeAcceptEdits {
		t.Fatalf("PermissionMode(nil) = %q, want the config's", got)
	}
}

func TestSetPermissionModeNilStatePanics(t *testing.T) {
	defer func() {
		if v := recover(); v == nil || !strings.Contains(fmt.Sprint(v), "nil State") {
			t.Fatalf("recovered %v, want a panic naming the nil State", v)
		}
	}()
	_ = SetPermissionMode(nil, permission.ModePlan)
}

func TestToolSelfCheckSeesRunMode(t *testing.T) {
	var mu sync.Mutex
	var modes []permission.Mode
	probe := tool.New("probe", "Probe.",
		func(context.Context, setModeInput, *tool.Call) (tool.Result, error) {
			return tool.Text("probed"), nil
		},
		tool.WithReadOnly(),
		tool.WithPermission(func(_ context.Context, _ json.RawMessage, pc permission.Context) permission.Decision {
			mu.Lock()
			defer mu.Unlock()
			modes = append(modes, pc.Mode)
			return permission.Allowed()
		}))
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{setModeCall("m"), {ID: "p", Name: "probe", Input: map[string]any{}}}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{setModeTool(), probe}})

	collect(t, a.Run(t.Context(), userInput("go")))

	mu.Lock()
	defer mu.Unlock()
	if len(modes) == 0 || modes[len(modes)-1] != permission.ModePlan {
		t.Fatalf("probe's CheckPermission saw modes %v, want plan last", modes)
	}
}

func TestCheckPermissionUsesStateMode(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New(), Tools: []tool.Tool{writeTool(newCallLog())}})
	s := state.New()
	if err := SetPermissionMode(s, permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	in := json.RawMessage(`{"text":"x"}`)

	if d := a.CheckPermission(t.Context(), &ToolCall{Name: "write_file", Input: in, State: s}); d.Behavior != permission.Deny {
		t.Fatalf("decision in plan mode = %+v, want deny", d)
	}
	if d := a.CheckPermission(t.Context(), &ToolCall{Name: "write_file", Input: in}); d.Behavior != permission.Ask {
		t.Fatalf("decision without State = %+v, want ask (the config's default mode)", d)
	}
}

func TestPermissionModeSurvivesCheckpoint(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	open := func(turns ...llmtest.Turn) *Agent {
		cp, err := file.New(dir, file.Config{})
		if err != nil {
			t.Fatal(err)
		}
		return mustNew(t, Config{
			Provider:       llmtest.New(turns...),
			Tools:          []tool.Tool{setModeTool(), writeTool(newCallLog())},
			PermissionMode: permission.ModeBypass,
			Checkpointer:   cp,
		})
	}
	prompt := func(a *Agent) *Final {
		th, err := a.Thread(ctx, "t")
		if err != nil {
			t.Fatal(err)
		}
		_, final := collect(t, th.Prompt(ctx, "go"))
		if final.Reason != ReasonCompleted {
			t.Fatalf("final = %+v, want completed", final)
		}
		return final
	}

	prompt(open(llmtest.Turn{ToolCalls: []llmtest.Call{setModeCall("m")}}, llmtest.Turn{Text: "planning"}))
	final := prompt(open(llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w")}}, llmtest.Turn{Text: "done"}))

	if text, isErr := toolResult(t, final.State, "w"); !isErr || text != planDenied {
		t.Fatalf("write_file after restore = %q (error %v), want %q", text, isErr, planDenied)
	}
}

func TestPermissionModeOf(t *testing.T) {
	s := state.New()
	if mode, ok := PermissionModeOf(s); mode != "" || ok {
		t.Fatalf("PermissionModeOf(unset) = (%q, %v), want (\"\", false)", mode, ok)
	}
	if err := SetPermissionMode(s, permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	if mode, ok := PermissionModeOf(s); mode != permission.ModePlan || !ok {
		t.Fatalf("PermissionModeOf after SetPermissionMode(plan) = (%q, %v), want (plan, true)", mode, ok)
	}
	if err := SetPermissionMode(s, ""); err != nil {
		t.Fatal(err)
	}
	if mode, ok := PermissionModeOf(s); mode != "" || ok {
		t.Fatalf("PermissionModeOf after SetPermissionMode(\"\") = (%q, %v), want (\"\", false)", mode, ok)
	}
}
