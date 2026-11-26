package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
)

func noopRun(context.Context, json.RawMessage, *Call) (Result, error) { return Text("ok"), nil }

func TestBuildFailClosedDefaults(t *testing.T) {
	tl := Build(Spec{Name: "danger", Run: noopRun})
	in := json.RawMessage(`{}`)

	if r, ok := As[ReadOnlyer](tl); !ok || r.ReadOnly(in) {
		t.Error("default tool must not be read-only")
	}
	if c, ok := As[ConcurrencySafer](tl); !ok || c.ConcurrencySafe(in) {
		t.Error("default tool must not be concurrency-safe")
	}
}

func TestBuildNilPermissionNoOpinion(t *testing.T) {
	tl := Build(Spec{Name: "a", Run: noopRun})
	pc, ok := As[PermissionChecker](tl)
	if !ok {
		t.Fatal("a built tool does not implement PermissionChecker")
	}
	d := pc.CheckPermission(t.Context(), json.RawMessage(`{}`), permission.Context{})
	if d.Behavior != "" || d.Message != "" || d.UpdatedInput != nil {
		t.Fatalf("CheckPermission = %+v, want the zero Decision", d)
	}
	got := permission.Evaluate(permission.Request{Tool: "a", SelfCheck: d}, permission.Context{Mode: permission.ModeDefault})
	if got.Behavior != permission.Ask || got.Message != "approve a?" {
		t.Fatalf("Evaluate = %+v, want Ask %q", got, "approve a?")
	}
}

func TestBuildDispatcherOnlyWhenSet(t *testing.T) {
	if _, ok := As[Dispatcher](Build(Spec{Name: "a", Run: noopRun})); ok {
		t.Fatal("a tool without Spec.Dispatch is a Dispatcher")
	}
	tl := Build(Spec{
		Name: "a",
		Run:  noopRun,
		Dispatch: func(in json.RawMessage) (string, json.RawMessage, error) {
			return "target", in, nil
		},
	})
	d, ok := As[Dispatcher](tl)
	if !ok {
		t.Fatal("a tool with Spec.Dispatch is not a Dispatcher")
	}
	name, in, err := d.Dispatch(json.RawMessage(`{"x":1}`))
	if err != nil || name != "target" || string(in) != `{"x":1}` {
		t.Fatalf("Dispatch = %q, %s, %v", name, in, err)
	}
	for _, check := range []bool{
		func() bool { _, ok := As[ReadOnlyer](tl); return ok }(),
		func() bool { _, ok := As[PathAccessor](tl); return ok }(),
		func() bool { _, ok := As[Availability](tl); return ok }(),
	} {
		if !check {
			t.Fatal("the dispatching tool lost a capability of built")
		}
	}
}

func TestBuildAvailable(t *testing.T) {
	a, ok := As[Availability](Build(Spec{Name: "a", Run: noopRun}))
	if !ok || a.Available(nil) != nil {
		t.Fatal("a tool without Spec.Available must be available")
	}
	locked := errors.New("locked")
	a, _ = As[Availability](Build(Spec{Name: "a", Run: noopRun, Available: func(*state.State) error { return locked }}))
	if err := a.Available(state.New()); !errors.Is(err, locked) {
		t.Fatalf("Available = %v, want %v", err, locked)
	}
}

// handTool implements Tool and several capabilities by hand.
type handTool struct{}

func (handTool) Name() string           { return "hand" }
func (handTool) Description() string    { return "By hand." }
func (handTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (handTool) Call(context.Context, json.RawMessage, *Call) (Result, error) {
	return Text("hand ran"), nil
}
func (handTool) Subject(json.RawMessage) string { return "rm -rf /" }
func (handTool) ShellSubject() bool             { return true }
func (handTool) Paths(json.RawMessage) []permission.PathAccess {
	return []permission.PathAccess{{Path: "/x", Op: permission.OpWrite}}
}
func (handTool) CheckPermission(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Denied("never")
}
func (handTool) Available(*state.State) error { return errors.New("hand is locked") }

func TestSpecOfRoundTrip(t *testing.T) {
	s := SpecOf(handTool{})
	s.Name = "b"
	tl := Build(s)
	in := json.RawMessage(`{}`)

	if tl.Name() != "b" || tl.Description() != "By hand." {
		t.Fatalf("Name, Description = %q, %q", tl.Name(), tl.Description())
	}
	if sub, ok := As[Subjecter](tl); !ok || sub.Subject(in) != "rm -rf /" {
		t.Error("Subject not preserved")
	}
	if sh, ok := As[ShellSubjecter](tl); !ok || !sh.ShellSubject() {
		t.Error("ShellSubject not preserved")
	}
	if p, ok := As[PathAccessor](tl); !ok || len(p.Paths(in)) != 1 || p.Paths(in)[0].Path != "/x" {
		t.Error("Paths not preserved")
	}
	pc, _ := As[PermissionChecker](tl)
	if d := pc.CheckPermission(t.Context(), in, permission.Context{}); d.Behavior != permission.Deny || d.Message != "never" {
		t.Errorf("CheckPermission = %+v, want the Deny", d)
	}
	if a, _ := As[Availability](tl); a.Available(state.New()) == nil || a.Available(state.New()).Error() != "hand is locked" {
		t.Error("Available not preserved")
	}
	if res, err := tl.Call(t.Context(), in, &Call{}); err != nil || res.Flatten() != "hand ran" {
		t.Errorf("Call = %+v, %v", res, err)
	}
	if _, ok := As[Dispatcher](tl); ok {
		t.Error("SpecOf invented a Dispatch")
	}
}

func TestSpecOfBuiltIsCopy(t *testing.T) {
	tl := Build(Spec{Name: "a", Run: noopRun, Schema: map[string]any{"type": "object"}})
	s := SpecOf(tl)
	s.Schema["x"] = "leaked"
	s.Name = "b"
	if _, leaked := tl.Schema()["x"]; leaked || tl.Name() != "a" {
		t.Fatalf("mutating SpecOf's result changed the tool: name %q, schema %v", tl.Name(), tl.Schema())
	}
	d := Build(Spec{Name: "d", Run: noopRun, Dispatch: func(json.RawMessage) (string, json.RawMessage, error) {
		return "a", nil, nil
	}})
	if SpecOf(d).Dispatch == nil {
		t.Fatal("SpecOf of a built dispatcher lost Dispatch")
	}
}

func TestBuildImplementsOptionalInterfaces(t *testing.T) {
	tl := Build(Spec{
		Name:    "edit",
		Run:     noopRun,
		Edit:    func(json.RawMessage) bool { return true },
		Subject: func(json.RawMessage) string { return "subj" },
		Paths: func(json.RawMessage) []permission.PathAccess {
			return []permission.PathAccess{{Path: "/a", Op: permission.OpWrite}}
		},
		RawInput: true,
	})
	in := json.RawMessage(`{}`)

	if !tl.(Editor).IsEdit(in) {
		t.Error("IsEdit should follow Spec.Edit")
	}
	if got := tl.(Subjecter).Subject(in); got != "subj" {
		t.Errorf("Subject = %q", got)
	}
	if got := tl.(PathAccessor).Paths(in); len(got) != 1 || got[0].Path != "/a" {
		t.Errorf("Paths = %+v", got)
	}
	if !tl.(RawInputer).AcceptsRawInput() {
		t.Error("AcceptsRawInput should follow Spec.RawInput")
	}
}

func TestWithShellSubject(t *testing.T) {
	tl := New("sh", "Run.", func(context.Context, struct{}, *Call) (Result, error) { return Text("ok"), nil },
		WithShellSubject(func(json.RawMessage) string { return "ls" }))
	if got := tl.(Subjecter).Subject(nil); got != "ls" {
		t.Errorf("Subject = %q, want %q", got, "ls")
	}
	if !tl.(ShellSubjecter).ShellSubject() {
		t.Error("WithShellSubject must mark the subject as a shell command line")
	}
	plain := New("plain", "Run.", func(context.Context, struct{}, *Call) (Result, error) { return Text("ok"), nil },
		WithSubject(func(json.RawMessage) string { return "x" }))
	if plain.(ShellSubjecter).ShellSubject() {
		t.Error("WithSubject alone must not mark a shell subject")
	}
}

func TestBuildNilOptionalFuncsAreInert(t *testing.T) {
	tl := Build(Spec{Name: "plain", Run: noopRun})
	in := json.RawMessage(`{}`)
	if tl.(Editor).IsEdit(in) || tl.(Subjecter).Subject(in) != "" || tl.(PathAccessor).Paths(in) != nil || tl.(RawInputer).AcceptsRawInput() {
		t.Error("nil optional funcs must yield zero values")
	}
}

func TestBuildPanicsOnProgrammingErrors(t *testing.T) {
	tests := map[string]Spec{
		"empty name":  {Name: "", Run: noopRun},
		"bad name":    {Name: "write (/x)", Run: noopRun},
		"missing run": {Name: "norun"},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Build should panic")
				}
			}()
			Build(spec)
		})
	}
}

func TestBuildDefaultsToEmptyObjectSchema(t *testing.T) {
	s := Build(Spec{Name: "noargs", Run: noopRun}).Schema()
	if s["type"] != "object" {
		t.Errorf("Schema = %v, want an object schema", s)
	}
}

func TestCallProgressIsNilSafe(t *testing.T) {
	var nilCall *Call
	nilCall.Progress("ignored")
	(&Call{}).Progress("ignored")

	var got string
	(&Call{Emit: func(m string) { got = m }}).Progress("hello")
	if got != "hello" {
		t.Errorf("Emit received %q", got)
	}
}

func TestResultHelpers(t *testing.T) {
	r := Errorf("bad %d", 7)
	if !r.IsError || r.Flatten() != "bad 7" {
		t.Errorf("Errorf = %+v", r)
	}
	if r := Text("hi"); r.IsError || r.Flatten() != "hi" {
		t.Errorf("Text = %+v", r)
	}
}

func TestResultJSONFieldNames(t *testing.T) {
	res := Errorf("bad")
	got, err := json.Marshal(res)
	if want := `{"content":[{"type":"text","text":"bad"}],"is_error":true}`; err != nil || string(got) != want {
		t.Fatalf("json.Marshal(Result) = %s, %v; want %s", got, err, want)
	}
}

func TestCallVisibleIsNilSafe(t *testing.T) {
	var nilCall *Call
	if !nilCall.Visible("/a") || !(&Call{}).Visible("/a") {
		t.Fatal("a call without ReadVisible hides paths")
	}
	c := &Call{ReadVisible: func(p string) bool { return p != "/hidden" }}
	if c.Visible("/hidden") || !c.Visible("/shown") {
		t.Fatal("Visible does not consult ReadVisible")
	}
}
