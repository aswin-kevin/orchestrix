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

func TestDefaultNamesIsCopy(t *testing.T) {
	names := DefaultNames()
	names[0] = "X"
	if got := DefaultNames()[0]; got != "PATH" {
		t.Fatalf("DefaultNames()[0] = %q after a caller mutation, want PATH", got)
	}
	want := []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TMPDIR", "TZ"}
	if got := DefaultNames(); !slices.Equal(got, want) {
		t.Fatalf("DefaultNames() = %v, want %v", got, want)
	}
}

func TestInvalidKey(t *testing.T) {
	for _, tt := range []struct {
		vars map[string]string
		key  string
		bad  bool
	}{
		{nil, "", false},
		{map[string]string{"A": "1", "B_2": "x=y"}, "", false},
		{map[string]string{"": "1"}, "", true},
		{map[string]string{"OK": "1", "A=B": "1"}, "A=B", true},
		{map[string]string{"A\x00": "1"}, "A\x00", true},
	} {
		key, bad := InvalidKey(tt.vars)
		if key != tt.key || bad != tt.bad {
			t.Errorf("InvalidKey(%q) = %q, %v; want %q, %v", tt.vars, key, bad, tt.key, tt.bad)
		}
	}
}
