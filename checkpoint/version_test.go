package checkpoint_test

import (
	"errors"
	"testing"

	"github.com/aswin-kevin/orchestrix/checkpoint"
)

func TestVersionHelpers(t *testing.T) {
	v := checkpoint.FormatVersion("g", 3)
	if v != "g:3" {
		t.Fatalf("FormatVersion = %q", v)
	}
	if g, n, ok := checkpoint.ParseVersion(v); g != "g" || n != 3 || !ok {
		t.Fatalf("ParseVersion = %q, %d, %v", g, n, ok)
	}
	for _, bad := range []checkpoint.Version{"", "g", ":3", "g:0", "g:x"} {
		if _, _, ok := checkpoint.ParseVersion(bad); ok {
			t.Errorf("ParseVersion(%q) ok, want false", bad)
		}
	}
	a, b := checkpoint.NewGeneration(), checkpoint.NewGeneration()
	if len(a) != 24 || len(b) != 24 || a == b {
		t.Fatalf("NewGeneration = %q, %q", a, b)
	}
}

func TestConflictWrapsErrConflict(t *testing.T) {
	for _, tc := range []struct {
		expected, current checkpoint.Version
		want              string
	}{
		{"g:1", "", `checkpoint: thread was changed by another writer: thread "t" has no checkpoint, expected version "g:1"`},
		{"", "g:2", `checkpoint: thread was changed by another writer: thread "t" already exists at version "g:2"`},
		{"g:1", "g:2", `checkpoint: thread was changed by another writer: thread "t" is at version "g:2", expected "g:1"`},
	} {
		err := checkpoint.Conflict("t", tc.expected, tc.current)
		if !errors.Is(err, checkpoint.ErrConflict) || err.Error() != tc.want {
			t.Errorf("Conflict(%q, %q) = %v, want %q", tc.expected, tc.current, err, tc.want)
		}
	}
}
