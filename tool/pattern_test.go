package tool

import (
	"strings"
	"testing"
)

func TestMatchName(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"mcp__github__*", "mcp__github__create_issue", true},
		{"mcp__github__*", "mcp__gitlab__create_issue", false},
		{"read_*", "read_file", true},
		{"read_*", "xread_file", false},
		{"*_file", "read_file", true},
		{"*_file", "read_files", false},
		{"*gpt-*", "org/gpt-5", true},
		{"gpt-*", "org/gpt-5", false},
		{"a/*", "a/b/c", true},
		{"a/b", "a//b", false},
		{"?s", "ls", true},
		{"?s", "s", false},
		{"?", "/", true},
		{"ls", "LS", false},
		{"*", "", true},
		{"*", "anything", true},
		{"", "", true},
		{"", "x", false},
		{"a.b", "axb", false},
		{"a.b", "a.b", true},
		{"a+b", "aab", false},
		{"a[b]", "ab", false},
		{"a[b]", "a[b]", true},
		{`a\*`, "a*", false},
		{"**", "x/y", true},
		{"é?", "éx", true},
	}
	for _, tt := range tests {
		if got := MatchName(tt.pattern, tt.name); got != tt.want {
			t.Errorf("MatchName(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestValidPattern(t *testing.T) {
	for _, p := range []string{"", "  ", "\t\n", "a\x00"} {
		err := ValidPattern(p)
		if err == nil || !strings.HasPrefix(err.Error(), "tool: ") {
			t.Errorf("ValidPattern(%q) = %v, want a tool: error", p, err)
		}
	}
	for _, p := range []string{"*", "mcp__*", "a b", "?"} {
		if err := ValidPattern(p); err != nil {
			t.Errorf("ValidPattern(%q) = %v, want nil", p, err)
		}
	}
}
