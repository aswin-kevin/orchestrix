package tool

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/aswin-kevin/orchestrix/permission"
)

// logged is a wrapper that adds nothing but exposes its inner tool.
type logged struct{ Tool }

func (l logged) Unwrap() Tool { return l.Tool }

// opaque is a wrapper without Unwrap: nothing of its inner tool is visible.
type opaque struct{ Tool }

// writable is a wrapper that overrides ReadOnly to false.
type writable struct{ Tool }

func (w writable) Unwrap() Tool                { return w.Tool }
func (writable) ReadOnly(json.RawMessage) bool { return false }

func TestAsWalksUnwrap(t *testing.T) {
	want := []permission.PathAccess{{Path: "/a", Op: permission.OpRead}}
	inner := Build(Spec{
		Name:     "inner",
		Run:      noopRun,
		ReadOnly: always,
		Paths:    func(json.RawMessage) []permission.PathAccess { return want },
	})
	w := logged{inner}

	r, ok := As[ReadOnlyer](w)
	if !ok || !r.ReadOnly(nil) {
		t.Fatalf("As[ReadOnlyer] = %v, %v; want the inner read-only answer", r, ok)
	}
	p, ok := As[PathAccessor](logged{w})
	if !ok || !slices.Equal(p.Paths(nil), want) {
		t.Fatalf("As[PathAccessor] through two wrappers = %v, %v; want the inner paths", p, ok)
	}
	if _, ok := As[ReadOnlyer](opaque{inner}); ok {
		t.Fatal("As found a capability through a wrapper without Unwrap")
	}
	if _, ok := As[ReadOnlyer](nil); ok {
		t.Fatal("As[ReadOnlyer](nil) reported ok")
	}
}

func TestAsPrefersOutermost(t *testing.T) {
	inner := Build(Spec{Name: "inner", Run: noopRun, ReadOnly: always})
	r, ok := As[ReadOnlyer](writable{inner})
	if !ok || r.ReadOnly(nil) {
		t.Fatalf("As[ReadOnlyer] = %v, %v; want the wrapper's false", r, ok)
	}
	if _, isWrapper := r.(writable); !isWrapper {
		t.Fatalf("As returned %T, want the outer writable", r)
	}
}

func TestCheckInputFindsRawInputerThroughUnwrap(t *testing.T) {
	inner := Build(Spec{
		Name:     "raw",
		Schema:   map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}},
		RawInput: true,
		Run:      noopRun,
	})
	in := json.RawMessage(`{"n":"str"}`)
	if err := CheckInput(logged{inner}, in); err != nil {
		t.Fatalf("CheckInput through Unwrap = %v, want raw input passed through", err)
	}
	if err := CheckInput(opaque{inner}, in); err == nil {
		t.Fatal("CheckInput through a wrapper without Unwrap skipped validation")
	}
}
