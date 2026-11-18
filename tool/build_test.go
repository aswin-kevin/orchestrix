package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aswin-kevin/orchestrix/permission"
)

func noopRun(context.Context, json.RawMessage, *Call) (Result, error) { return Text("ok"), nil }

func TestBuildFailClosedDefaults(t *testing.T) {
	tl := Build(Spec{Name: "danger", Run: noopRun})
	in := json.RawMessage(`{}`)

	if tl.ReadOnly(in) {
		t.Error("default tool must not be read-only")
	}
	if tl.ConcurrencySafe(in) {
		t.Error("default tool must not be concurrency-safe")
	}
	d := tl.CheckPermission(context.Background(), in, permission.Context{})
	if d.Behavior != permission.Ask || d.Message != "approve danger?" {
		t.Errorf("default permission = %+v, want Ask %q", d, "approve danger?")
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
