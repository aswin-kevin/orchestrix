package subagents

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// probeResult is what the probe tool observed from inside a parent run.
type probeResult struct {
	in    agent.Input
	child *agent.Agent
	err   error
	be    tool.Call
}

// probeRun runs a parent whose only turn calls a probe tool; the tool starts
// spec through l and records the prepared input. The parent runs on st with
// thread "t" and mw as extra parent middleware.
func probeRun(t *testing.T, l *Launcher, spec Subagent, st *state.State, mw ...agent.Middleware) probeResult {
	t.Helper()
	var res probeResult
	probe := tool.New("probe", "Probes.", func(ctx context.Context, _ struct{}, c *tool.Call) (tool.Result, error) {
		info, _ := agent.Info(ctx)
		res.child, res.err = l.Agent(info.Agent, spec)
		if res.err != nil {
			return tool.Text("x"), nil
		}
		res.in, res.err = l.Input(ctx, res.child, spec, "do it", "x1", c)
		res.be = *c
		return tool.Text("ok"), nil
	}, tool.WithReadOnly())
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "p1", Name: "probe", Input: obj{}}}},
		llmtest.Turn{Text: "done"},
	)
	a, err := agent.New(agent.Config{
		Provider: p, Tools: []tool.Tool{probe}, Middleware: mw, PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	evs, final := collect(t, context.Background(), a, agent.Input{
		ThreadID: "t", State: st, Backend: testutil.Memory(t), Messages: []llm.Message{llm.UserText("go")},
	})
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v)", final.Reason, final.Err)
	}
	if res.child == nil && res.err == nil {
		t.Fatalf("probe never ran: %+v", evs)
	}
	return res
}

func researcher() Subagent {
	return Subagent{
		Name: "researcher", Description: "Researches.", SystemPrompt: "you research",
		Provider: llmtest.New(llmtest.Turn{Text: "child done"}), Skills: []string{"pdf"},
	}
}

func TestLauncherMatchesTaskTool(t *testing.T) {
	sk := skillsMiddleware(t)
	st := state.New()
	mustSetMode(t, st, permission.ModePlan)
	skills.ScopeKey.Set(st, skills.Scope{Restrict: true, Allowed: []string{"pdf", "style"}})
	res := probeRun(t, NewLauncher(), researcher(), st, sk)
	if res.err != nil {
		t.Fatal(res.err)
	}
	in := res.in
	if mode, _ := agent.PermissionModeOf(in.State); mode != permission.ModePlan {
		t.Errorf("mode = %q, want plan", mode)
	}
	scope, _ := skills.ScopeKey.Get(in.State)
	if !scope.Restrict || !slices.Equal(scope.Allowed, []string{"pdf"}) {
		t.Errorf("scope = %+v, want restricted to pdf", scope)
	}
	if in.ThreadID != "t/researcher/x1" || !in.Ephemeral || in.AgentName != "researcher" {
		t.Errorf("input = %+v", in)
	}
	if in.Backend == nil || in.Backend != res.be.Backend {
		t.Error("child backend is not the call's backend")
	}
	if len(in.Messages) != 1 || in.Messages[0].Role != llm.RoleUser || in.Messages[0].Text() != "do it" {
		t.Errorf("messages = %+v", in.Messages)
	}
}

func TestLauncherInputNeedsRun(t *testing.T) {
	l := NewLauncher()
	if _, err := l.Input(context.Background(), nil, researcher(), "x", "id", &tool.Call{State: state.New()}); err == nil {
		t.Fatal("want an error outside a run")
	}
}

func TestLauncherCachesPerParent(t *testing.T) {
	l := NewLauncher()
	spec := Subagent{Name: "w", Description: "d", SystemPrompt: "p"}
	mk := func() *agent.Agent {
		a, err := agent.New(agent.Config{Provider: llmtest.New()})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	p1, p2 := mk(), mk()
	a1, err := l.Agent(p1, spec)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := l.Agent(p1, spec); again != a1 {
		t.Error("same parent and spec built a second agent")
	}
	if other, _ := l.Agent(p2, spec); other == a1 {
		t.Error("another parent shared the agent")
	}
}

// countMW counts BeforeModel calls.
type countMW struct {
	name string
	n    *atomic.Int32
}

func (c *countMW) Name() string { return c.name }
func (c *countMW) BeforeModel(context.Context, *state.State) error {
	c.n.Add(1)
	return nil
}

func TestLauncherExtraMiddleware(t *testing.T) {
	var n atomic.Int32
	extra := &countMW{name: "extra", n: &n}
	l := NewLauncher(extra)
	spec := Subagent{Name: "w", Description: "d", SystemPrompt: "p", Provider: llmtest.New(llmtest.Turn{Text: "hi"})}
	parent, _ := agent.New(agent.Config{Provider: llmtest.New()})
	child, err := l.Agent(parent, spec)
	if err != nil {
		t.Fatal(err)
	}
	_, final := collect(t, context.Background(), child, userInput("go"))
	if final.Reason != agent.ReasonCompleted || n.Load() != 1 {
		t.Fatalf("reason = %s, BeforeModel calls = %d", final.Reason, n.Load())
	}

	// A different value with the same name clashes.
	spec.Name = "w2"
	spec.Middleware = []agent.Middleware{&countMW{name: "extra", n: &n}}
	if _, err := l.Agent(parent, spec); err == nil {
		t.Fatal("want a clash error")
	}
}

func TestLauncherExtraAlreadyPresentByIdentity(t *testing.T) {
	var n atomic.Int32
	extra := &countMW{name: "extra", n: &n}
	l := NewLauncher(extra)
	// The parent already carries extra, as a background child does.
	parent, err := agent.New(agent.Config{Provider: llmtest.New(), Middleware: []agent.Middleware{extra}})
	if err != nil {
		t.Fatal(err)
	}
	spec := Subagent{Name: "w", Description: "d", SystemPrompt: "p", Provider: llmtest.New(llmtest.Turn{Text: "hi"})}
	child, err := l.Agent(parent, spec)
	if err != nil {
		t.Fatalf("same value must not clash: %v", err)
	}
	if got := len(child.Config().Middleware); got != 1 {
		t.Errorf("child middleware = %d, want 1 (not appended twice)", got)
	}
	_, final := collect(t, context.Background(), child, userInput("go"))
	if final.Reason != agent.ReasonCompleted || n.Load() != 1 {
		t.Fatalf("reason = %s, extra BeforeModel calls = %d, want 1", final.Reason, n.Load())
	}
}

// sliceMW is a middleware whose value holds a slice, so it is not comparable.
type sliceMW struct {
	name string
	tags []string
}

func (s sliceMW) Name() string { return s.name }

// wrapped has a comparable static type, but == on it panics when the
// embedded interface holds a sliceMW.
type wrapped struct{ agent.Middleware }

func TestLauncherUncomparableExtraDoesNotPanic(t *testing.T) {
	mk := func() agent.Middleware { return wrapped{sliceMW{name: "extra", tags: []string{"a"}}} }
	l := NewLauncher(mk())
	parent, err := agent.New(agent.Config{Provider: llmtest.New(), Middleware: []agent.Middleware{mk()}})
	if err != nil {
		t.Fatal(err)
	}
	spec := Subagent{Name: "w", Description: "d", SystemPrompt: "p"}
	if _, err := l.Agent(parent, spec); err == nil {
		t.Fatal("want a clash error for an uncomparable value with the same name")
	}
}

func TestLauncherInputNilCall(t *testing.T) {
	var gotErr error
	probe := tool.New("probe", "Probes.", func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
		_, gotErr = NewLauncher().Input(ctx, nil, researcher(), "x", "id", nil)
		return tool.Text("ok"), nil
	}, tool.WithReadOnly())
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "p1", Name: "probe", Input: obj{}}}},
		llmtest.Turn{Text: "done"},
	)
	a, err := agent.New(agent.Config{Provider: p, Tools: []tool.Tool{probe}, PermissionMode: permission.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, context.Background(), a, userInput("go"))
	if gotErr == nil {
		t.Fatal("want an error for a nil call")
	}
}

func TestLauncherInputStricterModeWins(t *testing.T) {
	st := state.New()
	mustSetMode(t, st, permission.ModeDontAsk)
	spec := researcher()
	spec.Skills, spec.PermissionMode = nil, permission.ModeBypass
	res := probeRun(t, NewLauncher(), spec, st)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if mode, _ := agent.PermissionModeOf(res.in.State); mode != permission.ModeDontAsk {
		t.Errorf("mode = %q, want dontAsk", mode)
	}
}

func TestLauncherAgentValidatesSpec(t *testing.T) {
	parent, _ := agent.New(agent.Config{Provider: llmtest.New()})
	if _, err := NewLauncher().Agent(parent, Subagent{Name: "w", Description: "d"}); err == nil {
		t.Fatal("want an error for a spec without SystemPrompt")
	}
}

func TestResolveSpecs(t *testing.T) {
	names := func(specs []Subagent) []string {
		var out []string
		for _, s := range specs {
			out = append(out, s.Name)
		}
		return out
	}
	custom := Subagent{Name: "a", Description: "d", SystemPrompt: "p"}
	got, err := ResolveSpecs(Config{Subagents: []Subagent{custom}})
	if err != nil || !slices.Equal(names(got), []string{GeneralPurpose, "a"}) {
		t.Fatalf("got %v, %v", names(got), err)
	}
	got, err = ResolveSpecs(Config{Subagents: []Subagent{custom}, DisableGeneralPurpose: true})
	if err != nil || !slices.Equal(names(got), []string{"a"}) {
		t.Fatalf("disabled: got %v, %v", names(got), err)
	}
	repl := Subagent{Name: GeneralPurpose, Description: "mine", SystemPrompt: "p"}
	got, err = ResolveSpecs(Config{Subagents: []Subagent{repl}})
	if err != nil || len(got) != 1 || got[0].Description != "mine" {
		t.Fatalf("replaced: got %+v, %v", got, err)
	}
	if slices.Contains(names(got), Fork) {
		t.Error("fork must never be included")
	}
	if _, err := ResolveSpecs(Config{Subagents: []Subagent{{Name: Fork, Description: "d", SystemPrompt: "p"}}}); err == nil {
		t.Error("a spec named fork must be rejected")
	}
	if _, err := ResolveSpecs(Config{Subagents: []Subagent{{Name: "bad name!", Description: "d", SystemPrompt: "p"}}}); err == nil {
		t.Error("want an error for an invalid spec")
	}
}

func TestChildConfigNeverCarriesOutputSchema(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}
	for _, tc := range []struct {
		name   string
		parent agent.Config
		spec   Subagent
	}{
		{"parent schema", agent.Config{OutputSchema: map[string]any{"type": "object"}}, Subagent{Name: "plain", SystemPrompt: "p"}},
		{"spec schema", agent.Config{}, Subagent{Name: "typed", SystemPrompt: "p", OutputSchema: schema}},
		{"both", agent.Config{OutputSchema: map[string]any{"type": "object"}}, Subagent{Name: "typed", SystemPrompt: "p", OutputSchema: schema}},
	} {
		cfg, err := childConfig(tc.parent, tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cfg.OutputSchema != nil {
			t.Errorf("%s: child OutputSchema = %v, want nil", tc.name, cfg.OutputSchema)
		}
	}
}

func TestChildConfigMaxTurns(t *testing.T) {
	for _, tc := range []struct {
		parent, spec, want int
	}{
		{parent: 7, spec: 0, want: 7},   // zero inherits
		{parent: 7, spec: 3, want: 3},   // positive bounds the child
		{parent: 7, spec: -1, want: -1}, // negative is unbounded
		{parent: 0, spec: -5, want: -5},
	} {
		cfg, err := childConfig(agent.Config{MaxTurns: tc.parent}, Subagent{Name: "s", SystemPrompt: "p", MaxTurns: tc.spec})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MaxTurns != tc.want {
			t.Errorf("parent %d, spec %d: child MaxTurns = %d, want %d", tc.parent, tc.spec, cfg.MaxTurns, tc.want)
		}
	}
}
