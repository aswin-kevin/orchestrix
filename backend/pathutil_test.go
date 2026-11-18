package backend

import (
	"errors"
	"testing"
)

func TestClean(t *testing.T) {
	tests := map[string]string{
		"/a/b":     "/a/b",
		"a/b":      "/a/b",
		"../x":     "/x",
		"/../x":    "/x",
		"/a//b/./": "/a/b",
		"/a/../b":  "/b",
		"/":        "/",
	}
	for in, want := range tests {
		got, err := Clean(in)
		if err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestCleanRejects(t *testing.T) {
	for _, in := range []string{"", "/a\x00b"} {
		if _, err := Clean(in); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Clean(%q) error = %v, want ErrInvalidPath", in, err)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	if !MatchGlob("**/*.go", "/src/pkg/a.go") {
		t.Error("**/*.go should match /src/pkg/a.go")
	}
	if MatchGlob("*.go", "/src/a.go") {
		t.Error("*.go should not cross directories")
	}
}
