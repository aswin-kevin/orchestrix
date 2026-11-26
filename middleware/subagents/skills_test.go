package subagents

import (
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/permission"
)

const styleBody = "Write short sentences."

func skillsMiddleware(t *testing.T) *skills.Middleware {
	t.Helper()
	m, err := skills.New(skills.Config{Skills: []skills.Skill{
		{Name: "pdf", Description: "PDFs.", Body: "pdf body"},
		{Name: "other", Description: "Other.", Body: "other body"},
		{Name: "style", Description: "Style.", Body: styleBody},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// runWithSkills runs a parent with the skills and subagents middleware that
// delegates once to typ, and returns the parent's final.
func runWithSkills(t *testing.T, sk *skills.Middleware, specs []Subagent, typ string) *agent.Final {
	t.Helper()
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", typ, "go")}},
		llmtest.Turn{Text: "done"},
	)
	mw := newMiddleware(t, Config{Subagents: specs})
	a, err := agent.New(agent.Config{
		Provider: parent, Middleware: []agent.Middleware{sk, mw}, PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, final := collect(t, t.Context(), a, userInput("go"))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v)", final.Reason, final.Err)
	}
	return final
}

// catalogOf returns the joined system prompt of a request.
func catalogOf(req llm.Request) string { return strings.Join(req.System, "\n") }

func TestSubagentSkillScope(t *testing.T) {
	sk := skillsMiddleware(t)
	scoped := llmtest.New(llmtest.Turn{Text: "scoped done"})
	plain := llmtest.New(llmtest.Turn{Text: "plain done"})
	specs := []Subagent{
		{Name: "pdfer", Description: "d", SystemPrompt: "p", Provider: scoped, Skills: []string{"pdf"}, PinnedSkills: []string{"style"}},
		{Name: "plain", Description: "d", SystemPrompt: "p", Provider: plain},
	}
	runWithSkills(t, sk, specs, "pdfer")
	sys := catalogOf(scoped.Requests[0])
	if !strings.Contains(sys, "- pdf: PDFs.") || strings.Contains(sys, "- other:") || strings.Contains(sys, "- style:") ||
		!strings.Contains(sys, styleBody) {
		t.Fatalf("scoped child system:\n%s", sys)
	}

	runWithSkills(t, sk, specs, "plain")
	sys = catalogOf(plain.Requests[0])
	if !strings.Contains(sys, "- pdf:") || !strings.Contains(sys, "- other:") || !strings.Contains(sys, "- style:") ||
		strings.Contains(sys, styleBody) {
		t.Fatalf("unscoped child system:\n%s", sys)
	}
}

func TestGrandchildKeepsSkillScope(t *testing.T) {
	for name, inner := range map[string][]string{
		"inherits":   nil,
		"intersects": {"pdf", "other"},
	} {
		t.Run(name, func(t *testing.T) {
			child := llmtest.New(
				llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("inner", "inner", "deeper")}},
				llmtest.Turn{Text: "child done"},
			)
			grandchild := llmtest.New(llmtest.Turn{Text: "grandchild done"})
			specs := []Subagent{
				{Name: "pdfer", Description: "d", SystemPrompt: "p", Provider: child, Skills: []string{"pdf"}, PinnedSkills: []string{"style"}},
				{Name: "inner", Description: "d", SystemPrompt: "p", Provider: grandchild, Skills: inner},
			}
			runWithSkills(t, skillsMiddleware(t), specs, "pdfer")
			if len(grandchild.Requests) == 0 {
				t.Fatal("the grandchild never ran")
			}
			sys := catalogOf(grandchild.Requests[0])
			if !strings.Contains(sys, "- pdf:") || strings.Contains(sys, "- other:") || !strings.Contains(sys, styleBody) {
				t.Fatalf("grandchild system:\n%s", sys)
			}
		})
	}
}

func TestUnknownSubagentSkillsFail(t *testing.T) {
	sk := skillsMiddleware(t)
	for _, spec := range []Subagent{
		{Name: "a", Description: "d", SystemPrompt: "p", Middleware: []agent.Middleware{sk}, Skills: []string{"ghost"}},
		{Name: "a", Description: "d", SystemPrompt: "p", Middleware: []agent.Middleware{sk}, PinnedSkills: []string{"ghost"}},
		{Name: "a", Description: "d", SystemPrompt: "p", Middleware: []agent.Middleware{}, Skills: []string{"pdf"}},
	} {
		if _, err := New(Config{Subagents: []Subagent{spec}}); err == nil {
			t.Errorf("New(%+v) succeeded, want an error", spec)
		}
	}

	// A subagent inheriting the parent's middleware is checked when built.
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "a", "go")}},
		llmtest.Turn{Text: "done"},
	)
	mw := newMiddleware(t, Config{Subagents: []Subagent{{Name: "a", Description: "d", SystemPrompt: "p", Skills: []string{"ghost"}}}})
	a, err := agent.New(agent.Config{Provider: parent, Middleware: []agent.Middleware{sk, mw}, PermissionMode: permission.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, t.Context(), a, userInput("go"))
	if res := onlyResult(t, parent); !res.IsError || !strings.Contains(blockText(res), "ghost") {
		t.Fatalf("task result = %q (error %v)", blockText(res), res.IsError)
	}
}

func TestCheckSkillsUsesInheritedStack(t *testing.T) {
	sk := skillsMiddleware(t)
	inherited := []agent.Middleware{sk}
	for _, tt := range []struct {
		spec    Subagent
		wantErr string
	}{
		{Subagent{Name: "a", Skills: []string{"pdf"}, PinnedSkills: []string{"style"}}, ""},
		{Subagent{Name: "a", Skills: []string{"ghost"}}, "ghost"},
		{Subagent{Name: "a", PinnedSkills: []string{"ghost"}}, "ghost"},
		// Its own stack wins over the inherited one.
		{Subagent{Name: "a", Middleware: []agent.Middleware{}, Skills: []string{"pdf"}}, "skills middleware"},
		{Subagent{Name: "a"}, ""},
	} {
		err := tt.spec.CheckSkills(inherited)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("CheckSkills(%+v) = %v, want nil", tt.spec, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("CheckSkills(%+v) = %v, want an error mentioning %q", tt.spec, err, tt.wantErr)
		}
	}
	if err := (Subagent{Name: "a", Skills: []string{"pdf"}}).CheckSkills(nil); err == nil {
		t.Error("CheckSkills without a skills middleware succeeded")
	}
}
