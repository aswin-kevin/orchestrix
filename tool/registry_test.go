package tool

import (
	"testing"
)

func named(name string) Tool { return Build(Spec{Name: name, Description: "d " + name, Run: noopRun}) }

func TestRegistryDuplicate(t *testing.T) {
	if _, err := NewRegistry(named("a"), named("b"), named("a")); err == nil {
		t.Fatal("NewRegistry should reject duplicate names")
	}
	r, err := NewRegistry(named("a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(named("a")); err == nil {
		t.Error("Add should reject a duplicate")
	}
	if err := r.Add(nil); err == nil {
		t.Error("Add should reject a nil tool")
	}
}

func TestRegistryOrderLookupAndSchemas(t *testing.T) {
	r, err := NewRegistry(named("zeta"), named("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(named("mid")); err != nil {
		t.Fatal(err)
	}

	var order []string
	for _, tl := range r.Tools() {
		order = append(order, tl.Name())
	}
	if got := order; len(got) != 3 || got[0] != "zeta" || got[1] != "alpha" || got[2] != "mid" {
		t.Errorf("Tools order = %v, want insertion order", got)
	}
	if tl, ok := r.Get("alpha"); !ok || tl.Name() != "alpha" {
		t.Error("Get(alpha) failed")
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) should report false")
	}
	schemas := r.Schemas()
	if len(schemas) != 3 || schemas[2].Name != "mid" || schemas[2].Description != "d mid" || schemas[2].InputSchema["type"] != "object" {
		t.Errorf("Schemas = %+v", schemas)
	}
}
