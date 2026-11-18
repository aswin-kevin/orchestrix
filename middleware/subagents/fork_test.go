package subagents

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

func TestForkReusesParentPrefix(t *testing.T) {
	// Parent and fork are the same agent on the same provider; the fork runs
	// during the parent's tool turn, so the script order is deterministic.
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{
			taskCall("t1", Fork, "summarize the findings"),
			{ID: "e1", Name: "echo", Input: obj{"text": "hi"}},
		}},
		llmtest.Turn{Text: "fork result"},
		llmtest.Turn{Text: "done"},
	)
	_, final := collect(t, context.Background(), newParent(t, p, newMiddleware(t, Config{}), echoTool()), userInput("parent history"))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	if p.Calls() != 3 {
		t.Fatalf("provider got %d requests, want parent, fork, parent", p.Calls())
	}
	first, fork, last := p.Requests[0], p.Requests[1], p.Requests[2]

	if !slices.Equal(fork.System, first.System) {
		t.Fatalf("fork system = %q, want the parent's %q", fork.System, first.System)
	}
	if !slices.Equal(toolNames(fork), toolNames(first)) {
		t.Fatalf("fork tools = %v, want the parent's %v", toolNames(fork), toolNames(first))
	}
	// The parent's last request is its history, the assistant message with
	// the task call, then its own tool results; the fork shares all but the
	// results.
	shared := len(last.Messages) - 1
	if len(fork.Messages) != len(last.Messages) || !reflect.DeepEqual(fork.Messages[:shared], last.Messages[:shared]) {
		t.Fatalf("fork messages = %+v\nwant the parent's history %+v as prefix", fork.Messages, last.Messages[:shared])
	}
	results := toolResults(fork)
	if len(results) != 2 || results[0].ToolUseID != "t1" || results[1].ToolUseID != "e1" {
		t.Fatalf("fork tool results = %+v, want one for t1 and one for e1", results)
	}
	if got := blockText(results[0]); got != ForkPrefix+"summarize the findings" {
		t.Fatalf("task result in fork = %q, want ForkPrefix + description", got)
	}
	if got := blockText(results[1]); got != "(handled by the parent agent)" {
		t.Fatalf("sibling result in fork = %q, want the parent-handled note", got)
	}
	if res := toolResults(last); blockText(res[0]) != "fork result" {
		t.Fatalf("task result = %q, want the fork's final text", blockText(res[0]))
	}
}

func TestForkCannotDelegate(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", Fork, "do it")}},
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("inner", GeneralPurpose, "delegate again")}},
		llmtest.Turn{Text: "fork did it itself"},
		llmtest.Turn{Text: "done"},
	)
	_, final := collect(t, context.Background(), newParent(t, p, newMiddleware(t, Config{})), userInput("go"))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	if p.Calls() != 4 {
		t.Fatalf("provider got %d requests, want 4 (no grandchild ran)", p.Calls())
	}
	results := toolResults(p.Requests[2])
	if len(results) != 1 || !results[0].IsError || blockText(results[0]) != "a forked subagent cannot delegate" {
		t.Fatalf("nested task result = %+v, want the cannot-delegate error", results)
	}
	if res := toolResults(p.Requests[3]); blockText(res[0]) != "fork did it itself" {
		t.Fatalf("task result = %q, want the fork's final text", blockText(res[0]))
	}
}

func TestForkStateMergesBack(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", Fork, "write it")}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w", Name: "writer"}}},
		llmtest.Turn{Text: "written"},
		llmtest.Turn{Text: "done"},
	)
	be := memory.New()
	a, err := agent.New(agent.Config{
		Provider: p, Backend: be, Tools: []tool.Tool{writerTool()},
		Middleware: []agent.Middleware{newMiddleware(t, Config{})}, PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, final := collect(t, context.Background(), a, userInput("go"))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	if res := toolResults(p.Requests[2]); len(res) != 1 || res[0].IsError {
		t.Fatalf("writer result in fork = %+v, want success", res)
	}
	if got, err := be.Read(context.Background(), "/out.md", 0, 0); err != nil || !slices.Equal(got.Lines, []string{"report"}) {
		t.Fatalf("parent backend /out.md = %+v, %v; want the fork's file", got, err)
	}
	if v, _ := sharedKey.Get(final.State); v != "from child" {
		t.Fatalf("shared key = %q, want the fork's value merged back", v)
	}
	if v, ok := privateKey.Get(final.State); ok {
		t.Fatalf("private key = %q, want the fork's private value kept out", v)
	}
	if _, ok := forkedKey.Get(final.State); ok {
		t.Fatal("the parent's state is marked forked")
	}
}

func TestForkNotOfferedWhenDisabled(t *testing.T) {
	task := newMiddleware(t, Config{DisableFork: true}).(agent.ToolProvider).Tools()[0]
	if strings.Contains(task.Description(), "- "+Fork+":") {
		t.Fatalf("task description lists fork although disabled:\n%s", task.Description())
	}
	enabled := newMiddleware(t, Config{}).(agent.ToolProvider).Tools()[0]
	if !strings.Contains(enabled.Description(), "- "+Fork+":") {
		t.Fatalf("task description does not list fork:\n%s", enabled.Description())
	}
}

// runLocalKey stands for a middleware's per-run bookkeeping, such as the
// background jobs a run started.
var runLocalKey = state.NewKey[string]("subagents_test.run_local", state.RunLocal[string]())

func TestForkStateDropsRunLocalValues(t *testing.T) {
	s := state.New()
	s.AppendMessages(llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockToolUse, ID: "t1", Name: ToolTask}}})
	runLocalKey.Set(s, "parent's")
	privateKey.Set(s, "kept")
	agent.WorkspaceKey.Set(s, json.RawMessage(`{}`))
	suspendedKey.Set(s, map[string]Suspended{"t0": {}})

	fork, err := forkState(&tool.Call{ToolUseID: "t1", State: s}, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := runLocalKey.Get(fork); ok {
		t.Fatalf("fork state holds the parent's run-local value %q", v)
	}
	if v, _ := privateKey.Get(fork); v != "kept" {
		t.Fatalf("fork private value = %q, want the parent's conversation state kept", v)
	}
	if _, ok := agent.WorkspaceKey.Get(fork); ok {
		t.Fatal("fork state holds the parent's workspace snapshot")
	}
	if _, ok := suspendedKey.Get(fork); ok {
		t.Fatal("fork state holds the parent's suspended subagents")
	}
}
