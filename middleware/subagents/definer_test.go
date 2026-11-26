package subagents

import (
	"context"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/permission"
)

func TestName(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if Name != "subagents" || m.Name() != Name {
		t.Fatalf("Name = %q, m.Name() = %q", Name, m.Name())
	}
}

// TestRefusedChildIsErrorResult reports a child's refusal as an error result
// carrying the child's reason.
func TestRefusedChildIsErrorResult(t *testing.T) {
	child := llmtest.New(llmtest.Turn{Text: "I can't help with that.", StopReason: llm.StopRefusal})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "researcher", "go")}},
		llmtest.Turn{Text: "done"},
	)
	mw := newMiddleware(t, Config{Subagents: []Subagent{{Name: "researcher", Description: "d", SystemPrompt: "p", Provider: child}}})
	a := newParent(t, parent, mw)
	collect(t, context.Background(), a, userInput("go"))
	res := onlyResult(t, parent)
	if want := "subagent researcher refused: I can't help with that."; !res.IsError || blockText(res) != want {
		t.Fatalf("task result = %q (error %v), want error %q", blockText(res), res.IsError, want)
	}
}

// definerMW is a user middleware named like the skills middleware that
// implements skills.Definer.
type definerMW struct{ names []string }

func (definerMW) Name() string { return skills.Name }
func (d definerMW) Has(name string) bool {
	for _, n := range d.names {
		if n == name {
			return true
		}
	}
	return false
}

// namedMW is a middleware named like the skills middleware without Has.
type namedMW struct{}

func (namedMW) Name() string { return skills.Name }

var _ skills.Definer = definerMW{}

func TestCustomSkillsDefiner(t *testing.T) {
	child := llmtest.New(llmtest.Turn{Text: "child done"})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "a", "go")}},
		llmtest.Turn{Text: "done"},
	)
	spec := Subagent{
		Name: "a", Description: "d", SystemPrompt: "p", Provider: child,
		Middleware: []agent.Middleware{definerMW{names: []string{"x"}}}, Skills: []string{"x"},
	}
	mw := newMiddleware(t, Config{Subagents: []Subagent{spec}})
	a, err := agent.New(agent.Config{Provider: parent, Middleware: []agent.Middleware{mw}, PermissionMode: permission.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, context.Background(), a, userInput("go"))
	if res := onlyResult(t, parent); res.IsError || blockText(res) != "child done" {
		t.Fatalf("task result = %q (error %v)", blockText(res), res.IsError)
	}

	spec.Skills = []string{"ghost"}
	if _, err := New(Config{Subagents: []Subagent{spec}}); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("New with an unknown skill = %v, want an error naming it", err)
	}
}

func TestSkillsWithoutDefinerFails(t *testing.T) {
	spec := Subagent{
		Name: "a", Description: "d", SystemPrompt: "p",
		Middleware: []agent.Middleware{namedMW{}}, Skills: []string{"x"},
	}
	_, err := New(Config{Subagents: []Subagent{spec}})
	want := `subagents: subagent "a" names skills or pinned skills, but no skills middleware implements skills.Definer`
	if err == nil || err.Error() != want {
		t.Fatalf("New = %v, want %q", err, want)
	}
}

func TestLauncherCheckpointer(t *testing.T) {
	spec := Subagent{Name: "w", Description: "d", SystemPrompt: "p"}
	parent, err := agent.New(agent.Config{Provider: llmtest.New(), Checkpointer: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	saved := memory.New()
	var gotParent *agent.Agent
	l := mustLauncher(t, LauncherConfig{Checkpointer: func(p *agent.Agent) checkpoint.Checkpointer {
		gotParent = p
		return saved
	}})
	child, err := l.Agent(parent, spec)
	if err != nil {
		t.Fatal(err)
	}
	if child.Config().Checkpointer != saved || gotParent != parent {
		t.Errorf("child checkpointer = %v (parent %p), want %v (parent %p)", child.Config().Checkpointer, gotParent, saved, parent)
	}

	for name, l := range map[string]*Launcher{
		"nil func":   mustLauncher(t, LauncherConfig{}),
		"nil result": mustLauncher(t, LauncherConfig{Checkpointer: func(*agent.Agent) checkpoint.Checkpointer { return nil }}),
		"typed nil result": mustLauncher(t, LauncherConfig{Checkpointer: func(*agent.Agent) checkpoint.Checkpointer {
			var none *memory.Checkpointer
			return none
		}}),
	} {
		child, err := l.Agent(parent, spec)
		if err != nil {
			t.Fatal(err)
		}
		if child.Config().Checkpointer != nil {
			t.Errorf("%s: child checkpointer = %v, want nil (the parent's is not inherited)", name, child.Config().Checkpointer)
		}
	}
}
