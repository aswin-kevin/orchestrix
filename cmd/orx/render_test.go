package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
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

func TestRenderSubagentPrefix(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newRenderer(&out, &errOut)

	r.render(agent.Event{Kind: agent.KindText, Agent: agent.DefaultAgentName, Text: "top\n"})
	r.render(agent.Event{Kind: agent.KindText, Agent: "researcher", Text: "found it\nline two\n"})
	r.render(agent.Event{Kind: agent.KindToolUse, Agent: "researcher", ToolName: "ls", Input: json.RawMessage(`{}`)})

	if want := "top\n  [researcher] found it\n  [researcher] line two\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if want := "  [researcher] ⏺ ls({})\n"; errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
}

func TestPromptEndsStreamedLine(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newRenderer(&out, &errOut)
	r.render(agent.Event{Kind: agent.KindText, Agent: agent.DefaultAgentName, Text: "partial"})

	fmt.Fprint(r.prompts(), "Allow x? ")

	if want := "partial\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q (open line ended before the prompt)", out.String(), want)
	}
	if errOut.String() != "Allow x? " {
		t.Errorf("stderr = %q, want the prompt", errOut.String())
	}
}
