package hostenv

import (
	"os"
	"slices"
	"testing"
)

func TestInheritedAllowlist(t *testing.T) {
	t.Setenv("HOSTENV_TEST_KEEP", "kept")
	t.Setenv("HOSTENV_TEST_DROP", "dropped")
	got := Inherited([]string{"HOSTENV_TEST_KEEP", "HOSTENV_TEST_UNSET"}, false)
	if want := []string{"HOSTENV_TEST_KEEP=kept"}; !slices.Equal(got, want) {
		t.Fatalf("Inherited = %v, want %v", got, want)
	}
}

func TestInheritedAll(t *testing.T) {
	t.Setenv("HOSTENV_TEST_DROP", "present")
	got := Inherited(nil, true)
	if !slices.Contains(got, "HOSTENV_TEST_DROP=present") || len(got) != len(os.Environ()) {
		t.Fatalf("Inherited(all) does not match os.Environ: %d vs %d entries", len(got), len(os.Environ()))
	}
}

func TestPairsSortedByKey(t *testing.T) {
	got := Pairs(map[string]string{"B": "2", "A": "1", "C": ""})
	if want := []string{"A=1", "B=2", "C="}; !slices.Equal(got, want) {
		t.Fatalf("Pairs = %v, want %v", got, want)
	}
	if Pairs(nil) != nil {
		t.Fatal("Pairs(nil) is not nil")
	}
}
