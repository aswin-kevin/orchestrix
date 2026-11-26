package pathutil

import (
	"errors"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
)

func TestCleanBase(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		invalid bool
	}{
		{"empty is root", "", "/", false},
		{"root", "/", "/", false},
		{"dotdot", "a/../b", "/b", false},
		{"relative", "a/b/", "/a/b", false},
		{"nul byte", "a\x00b", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CleanBase(tt.in)
			if tt.invalid {
				if !errors.Is(err, backend.ErrInvalidPath) {
					t.Fatalf("CleanBase(%q) error = %v, want ErrInvalidPath", tt.in, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("CleanBase(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
			if tt.in != "" {
				if want, _ := backend.Clean(tt.in); got != want {
					t.Fatalf("CleanBase(%q) = %q, backend.Clean = %q", tt.in, got, want)
				}
			}
		})
	}
}

func TestFail(t *testing.T) {
	err := Fail("kv", "read", "/a", backend.ErrNotFound)
	if !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Fail does not wrap the sentinel: %v", err)
	}
	if got, want := err.Error(), "kv: read /a: "+backend.ErrNotFound.Error(); got != want {
		t.Fatalf("Fail = %q, want %q", got, want)
	}
}
