package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"short unchanged", "abc", 5, "abc"},
		{"exact length unchanged", "abcde", 5, "abcde"},
		{"long cut with ellipsis", "abcdefgh", 5, "abcd…"},
		{"counts runes not bytes", "ééééé", 3, "éé…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncate(tc.in, tc.limit); got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	tests := map[string]string{"": "", "one": "one", "one\ntwo": "one", "\ntwo": ""}
	for in, want := range tests {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompactJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"whitespace removed", "{ \"a\": 1,\n \"b\": [2, 3] }", `{"a":1,"b":[2,3]}`},
		{"invalid json falls back to first line", "not json\nmore", "not json"},
		{"long input truncated", `"` + strings.Repeat("x", 200) + `"`, `"` + strings.Repeat("x", maxLineRunes-2) + "…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactJSON(json.RawMessage(tc.in)); got != tc.want {
				t.Errorf("compactJSON = %q, want %q", got, tc.want)
			}
		})
	}
}
