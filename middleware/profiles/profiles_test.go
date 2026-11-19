package profiles

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/tool"
)

type obj = map[string]any

func stub(name, description string) tool.Tool {
	return tool.New(name, description,
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
			return tool.Text(name + " ran"), nil
		}, tool.WithReadOnly())
}

func mustProfiles(t *testing.T, adjs ...Adjustment) *Middleware {
	t.Helper()
	m, err := New(Config{Adjustments: adjs})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// runAgent builds an agent and runs it once; it returns the final event.
func runAgent(t *testing.T, cfg agent.Config) *agent.Final {
	t.Helper()
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var final *agent.Final
	for ev, err := range a.Run(context.Background(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	return final
}

func schemaNames(req llm.Request) []string {
	var out []string
	for _, s := range req.Tools {
		out = append(out, s.Name)
	}
	return out
}

func firstResult(t *testing.T, final *agent.Final) llm.Block {
	t.Helper()
	return final.State.Messages()[2].Content[0]
}

func TestPromptSuffixForMatchingModel(t *testing.T) {
	m := mustProfiles(t, Adjustment{Models: []string{"big-*"}, PromptSuffix: "Be terse."})
	for model, want := range map[string]bool{"big-5": true, "other-x": false} {
		p := llmtest.New(llmtest.Turn{Text: "ok"})
		runAgent(t, agent.Config{Provider: p, Model: model, System: []string{"base"}, Middleware: []agent.Middleware{m}})
		sys := p.Requests[0].System
		if got := sys[len(sys)-1] == "Be terse."; got != want {
			t.Fatalf("model %s: suffix last = %v, want %v (system %q)", model, got, want, sys)
		}
	}
}

func TestSuffixesAppendInOrderAndOverridesLaterWins(t *testing.T) {
	m := mustProfiles(t,
		Adjustment{Models: []string{"big-*"}, PromptSuffix: "one", ToolDescriptions: map[string]string{"a": "first"}},
		Adjustment{Models: []string{"*-1"}, PromptSuffix: "two", ToolDescriptions: map[string]string{"a": "second"}},
		Adjustment{Models: []string{"nomatch"}, PromptSuffix: "three"},
	)
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	runAgent(t, agent.Config{Provider: p, Model: "big-1", Tools: []tool.Tool{stub("a", "own")}, Middleware: []agent.Middleware{m}})
	req := p.Requests[0]
	n := len(req.System)
	if !slices.Equal(req.System[n-2:], []string{"one", "two"}) {
		t.Fatalf("system = %q, want suffixes one, two last", req.System)
	}
	if req.Tools[0].Description != "second" {
		t.Fatalf("description = %q, want second", req.Tools[0].Description)
	}
}

func TestToolDescriptionOverride(t *testing.T) {
	m := mustProfiles(t, Adjustment{Models: []string{"big-*"}, ToolDescriptions: map[string]string{"write_file": "Short."}})
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	orig := stub("write_file", "A long original description.")
	runAgent(t, agent.Config{Provider: p, Model: "big-1", Tools: []tool.Tool{orig, stub("read_file", "Reads.")}, Middleware: []agent.Middleware{m}})
	got := p.Requests[0].Tools
	if got[0].Name != "write_file" || got[0].Description != "Short." {
		t.Fatalf("write_file = %+v", got[0])
	}
	if got[1].Description != "Reads." {
		t.Fatalf("read_file description changed: %q", got[1].Description)
	}
	if want := orig.Schema(); len(got[0].InputSchema) != len(want) {
		t.Fatalf("schema changed: %v vs %v", got[0].InputSchema, want)
	}
	if orig.Description() != "A long original description." {
		t.Fatal("the tool itself was modified")
	}
}

func TestExcludedToolHiddenAndRefused(t *testing.T) {
	m := mustProfiles(t, Adjustment{Models: []string{"small-*"}, ExcludeTools: []string{"execute"}})
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: "execute"}}},
		llmtest.Turn{Text: "done"},
	)
	final := runAgent(t, agent.Config{Provider: p, Model: "small-1", Tools: []tool.Tool{stub("execute", "Runs."), stub("read", "Reads.")}, Middleware: []agent.Middleware{m}})
	if got := schemaNames(p.Requests[0]); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("schemas = %v, want read only", got)
	}
	res := firstResult(t, final)
	if !res.IsError || res.Content[0].Text != "tool execute is not available for model small-1" {
		t.Fatalf("result = %+v", res)
	}
}

func TestExcludedToolRefusedViaCallTool(t *testing.T) {
	m := mustProfiles(t, Adjustment{Models: []string{"small-*"}, ExcludeTools: []string{"execute"}})
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: deferred.ToolCall, Input: obj{"name": "execute", "input": obj{}}}}},
		llmtest.Turn{Text: "done"},
	)
	final := runAgent(t, agent.Config{
		Provider: p, Model: "small-1", Tools: []tool.Tool{stub("execute", "Runs.")},
		Middleware: []agent.Middleware{deferred.New(deferred.Config{Tools: []string{"execute"}}), m},
	})
	res := firstResult(t, final)
	if !res.IsError || res.Content[0].Text != "tool execute is not available for model small-1" {
		t.Fatalf("result = %+v", res)
	}
}

func TestExcludeMiddlewareAtNew(t *testing.T) {
	m := mustProfiles(t, Adjustment{Models: []string{"small-*"}, ExcludeMiddleware: []string{"planning"}})
	for model, want := range map[string]bool{"small-1": false, "big-1": true} {
		a, err := agent.New(agent.Config{
			Provider: llmtest.New(), Model: model,
			Middleware: []agent.Middleware{m, planning.New(planning.Config{})},
		})
		if err != nil {
			t.Fatal(err)
		}
		has := slices.ContainsFunc(a.Tools(), func(tl tool.Tool) bool { return tl.Name() == planning.ToolWriteTodos })
		if has != want {
			t.Fatalf("model %s: has write_todos = %v, want %v", model, has, want)
		}
	}
}

func TestSubagentWithOtherModelRefiltered(t *testing.T) {
	m := mustProfiles(t, Adjustment{Models: []string{"small-*"}, ExcludeMiddleware: []string{"planning"}})
	child := llmtest.New(llmtest.Turn{Text: "child done"})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "t1", Name: subagents.ToolTask, Input: obj{"description": "do it", "subagent_type": "worker"}}}},
		llmtest.Turn{Text: "done"},
	)
	sub, err := subagents.New(subagents.Config{Subagents: []subagents.Subagent{{
		Name: "worker", Description: "Works.", SystemPrompt: "You work.", Provider: child, Model: "small-1",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	final := runAgent(t, agent.Config{
		Provider: parent, Model: "big-1",
		Middleware: []agent.Middleware{m, planning.New(planning.Config{}), sub},
	})
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v)", final.Reason, final.Err)
	}
	if !slices.Contains(schemaNames(parent.Requests[0]), planning.ToolWriteTodos) {
		t.Fatal("parent (big-1) lacks write_todos")
	}
	if slices.Contains(schemaNames(child.Requests[0]), planning.ToolWriteTodos) {
		t.Fatal("child (small-1) has write_todos")
	}
}

func TestParentWithoutPlanningChildHasIt(t *testing.T) {
	m := mustProfiles(t, Adjustment{Models: []string{"small-*"}, ExcludeMiddleware: []string{"planning"}})
	child := llmtest.New(llmtest.Turn{Text: "child done"})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "t1", Name: subagents.ToolTask, Input: obj{"description": "do it", "subagent_type": "worker"}}}},
		llmtest.Turn{Text: "done"},
	)
	// The subagent inherits the parent's Config.Middleware, which stays
	// unfiltered, and is filtered again for its own model.
	sub, err := subagents.New(subagents.Config{Subagents: []subagents.Subagent{{
		Name: "worker", Description: "Works.", SystemPrompt: "You work.", Provider: child, Model: "big-1",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	runAgent(t, agent.Config{
		Provider: parent, Model: "small-1",
		Middleware: []agent.Middleware{m, planning.New(planning.Config{}), sub},
	})
	if slices.Contains(schemaNames(parent.Requests[0]), planning.ToolWriteTodos) {
		t.Fatal("parent (small-1) has write_todos")
	}
	if !slices.Contains(schemaNames(child.Requests[0]), planning.ToolWriteTodos) {
		t.Fatal("child (big-1) lacks write_todos")
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name string
		adj  Adjustment
		want string
	}{
		{"no models", Adjustment{}, "no Models"},
		{"empty pattern", Adjustment{Models: []string{""}}, "invalid pattern"},
		{"blank pattern", Adjustment{Models: []string{"  "}}, "invalid pattern"},
		{"nul pattern", Adjustment{Models: []string{"a\x00b"}}, "invalid pattern"},
		{"bad tool pattern", Adjustment{Models: []string{"x"}, ExcludeTools: []string{""}}, "invalid pattern"},
		{"excludes itself", Adjustment{Models: []string{"x"}, ExcludeMiddleware: []string{"profiles"}}, "profiles"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{Adjustments: []Adjustment{tc.adj}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	if _, err := New(Config{}); err != nil {
		t.Fatalf("empty config: %v", err)
	}
}

func TestMatch(t *testing.T) {
	m := mustProfiles(t,
		Adjustment{Models: []string{"big-*"}, PromptSuffix: "a"},
		Adjustment{Models: []string{"small-?", "big-1"}, PromptSuffix: "b"},
	)
	if got := m.Match("big-1"); len(got) != 2 || got[0].PromptSuffix != "a" || got[1].PromptSuffix != "b" {
		t.Fatalf("Match(big-1) = %+v", got)
	}
	if got := m.Match("small-1"); len(got) != 1 || got[0].PromptSuffix != "b" {
		t.Fatalf("Match(small-1) = %+v", got)
	}
	if got := m.Match("other"); len(got) != 0 {
		t.Fatalf("Match(other) = %+v", got)
	}
}
