package agent

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// named is a middleware with a configurable name and an id to tell
// same-named instances apart.
type named struct {
	name string
	id   int
}

func (n named) Name() string { return n.name }

func names(mws []Middleware) []string {
	out := make([]string, len(mws))
	for i, m := range mws {
		out[i] = m.Name()
	}
	return out
}

func TestComposeReplaceInPlace(t *testing.T) {
	base := []Middleware{named{"a", 0}, named{"b", 0}, named{"c", 0}}
	user := []Middleware{named{"x", 1}, named{"b", 1}}

	got, err := Compose(base, user, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c", "x"}; !reflect.DeepEqual(names(got), want) {
		t.Fatalf("names = %v, want %v", names(got), want)
	}
	if got[1].(named).id != 1 {
		t.Fatal("user middleware did not replace base \"b\"")
	}
	if base[1].(named).id != 0 || len(base) != 3 {
		t.Fatal("Compose mutated its base input")
	}
}

func TestComposeExclude(t *testing.T) {
	base := []Middleware{named{"a", 0}, named{"b", 0}, named{"c", 0}}

	got, err := Compose(base, nil, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "c"}; !reflect.DeepEqual(names(got), want) {
		t.Fatalf("names = %v, want %v", names(got), want)
	}
}

func TestComposeErrors(t *testing.T) {
	base := []Middleware{named{"a", 0}}
	tests := []struct {
		name    string
		user    []Middleware
		exclude []string
		wantErr string
	}{
		{"unknown exclude", nil, []string{"nope"}, `"nope"`},
		{"duplicate user names", []Middleware{named{"x", 1}, named{"x", 2}}, nil, "duplicate"},
		{"nil user middleware", []Middleware{nil}, nil, "nil"},
		{"empty user name", []Middleware{named{"", 1}}, nil, "empty name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compose(base, tc.user, tc.exclude)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestComposeNilBaseEntry(t *testing.T) {
	if _, err := Compose([]Middleware{nil}, nil, nil); err == nil {
		t.Fatal("expected error for nil base middleware")
	}
}

// excluder removes the named middleware for models matching only.
type excluder struct {
	name   string
	only   string
	remove []string
}

func (e excluder) Name() string { return e.name }

func (e excluder) ExcludeMiddleware(model string) []string {
	if e.only != "" && e.only != model {
		return nil
	}
	return e.remove
}

// ownerStub is a middleware contributing one tool of the same name.
type ownerStub struct{ id string }

func (o ownerStub) Name() string { return o.id }

func (o ownerStub) Tools() []tool.Tool {
	return []tool.Tool{tool.New(o.id+"_tool", "A tool.",
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) { return tool.Text("ok"), nil })}
}

func newExcluded(t *testing.T, model string, mws ...Middleware) *Agent {
	t.Helper()
	a, err := New(Config{Provider: llmtest.New(llmtest.Turn{Text: "x"}), Model: model, Middleware: mws})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func exToolNames(a *Agent) []string {
	var out []string
	for _, tl := range a.Tools() {
		out = append(out, tl.Name())
	}
	return out
}

func TestExcluderRemovesNamed(t *testing.T) {
	a := newExcluded(t, "small-1", excluder{name: "ex", remove: []string{"planning"}}, ownerStub{id: "planning"}, ownerStub{id: "other"})
	if got := exToolNames(a); !reflect.DeepEqual(got, []string{"other_tool"}) {
		t.Fatalf("tools = %v, want only other_tool", got)
	}
	if got := names(a.stack); !reflect.DeepEqual(got, []string{"ex", "other"}) {
		t.Fatalf("stack = %v", got)
	}
}

func TestExcluderNeverRemovesItself(t *testing.T) {
	a := newExcluded(t, "small-1", excluder{name: "ex", remove: []string{"ex", "planning"}}, ownerStub{id: "planning"})
	if got := names(a.stack); !reflect.DeepEqual(got, []string{"ex"}) {
		t.Fatalf("stack = %v, want the excluder alone", got)
	}
}

func TestExcluderUnknownNamesIgnored(t *testing.T) {
	a := newExcluded(t, "small-1", excluder{name: "ex", remove: []string{"nope"}}, ownerStub{id: "planning"})
	if got := names(a.stack); !reflect.DeepEqual(got, []string{"ex", "planning"}) {
		t.Fatalf("stack = %v, want unchanged", got)
	}
}

func TestExcluderFiltersOnlyComposedStack(t *testing.T) {
	ex := excluder{name: "ex", only: "small-1", remove: []string{"planning"}}
	parent := newExcluded(t, "small-1", ex, ownerStub{id: "planning"})
	if got := exToolNames(parent); len(got) != 0 {
		t.Fatalf("parent tools = %v, want none", got)
	}
	cfg := parent.Config()
	if got := names(cfg.Middleware); !reflect.DeepEqual(got, []string{"ex", "planning"}) {
		t.Fatalf("Config().Middleware = %v, want unfiltered", got)
	}
	cfg.Model = "big-1"
	child, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := exToolNames(child); !reflect.DeepEqual(got, []string{"planning_tool"}) {
		t.Fatalf("child tools = %v, want planning_tool", got)
	}
}

// hider hides the tool names it holds.
type hider struct {
	id    string
	names []string
}

func (h hider) Name() string { return h.id }

func (h hider) HidesTool(_ context.Context, name string) bool {
	return slices.Contains(h.names, name)
}

// probe records what ToolHidden and RunInfo.Model report inside a run.
type probe struct {
	hidden map[string]bool
	model  string
}

func (p *probe) Name() string { return "probe" }

func (p *probe) BeforeAgent(ctx context.Context, _ *state.State) error {
	info, _ := Info(ctx)
	p.model = info.Model()
	p.hidden = map[string]bool{"a": ToolHidden(ctx, "a"), "b": ToolHidden(ctx, "b")}
	return nil
}

func TestToolHiddenAndRunInfoModel(t *testing.T) {
	p := &probe{}
	a := newExcluded(t, "small-1", hider{id: "h", names: []string{"a"}}, p)
	for _, err := range a.Run(context.Background(), Input{Messages: []llm.Message{llm.UserText("go")}}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !p.hidden["a"] || p.hidden["b"] {
		t.Fatalf("hidden = %v, want a only", p.hidden)
	}
	if p.model != "small-1" {
		t.Fatalf("model = %q", p.model)
	}
	if ToolHidden(context.Background(), "a") {
		t.Fatal("ToolHidden true outside a run")
	}
	if (RunInfo{}).Model() != "" {
		t.Fatal("zero RunInfo has a model")
	}
}
