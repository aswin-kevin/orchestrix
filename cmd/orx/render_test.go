package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/tool"
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

func TestRenderEscapesToolLines(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newRenderer(&out, &errOut)
	r.dim = true // the renderer's own styling must survive the escaping
	res := tool.Text("ok\x1b]52;c;eA==\x07 done")

	r.render(agent.Event{Kind: agent.KindToolUse, ToolName: "mcp__x__t\x1b[2J", Input: json.RawMessage(`{"a":"\u001b"}`)})
	r.render(agent.Event{Kind: agent.KindToolUse, ToolName: "t", Input: json.RawMessage("not json \x1b[31m")})
	r.render(agent.Event{Kind: agent.KindToolResult, ToolName: "t", ToolResult: &res})

	want := ansiDim + `⏺ mcp__x__t\x1b[2J({"a":"\u001b"})` + ansiReset + "\n" +
		ansiDim + `⏺ t(not json \x1b[31m)` + ansiReset + "\n" +
		ansiDim + `  ⎿ ok\x1b]52;c;eA==\x07 done` + ansiReset + "\n"
	if errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
}

func TestRenderEscapesStreamedTextPerDelta(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newRenderer(&out, &errOut)

	// An escape sequence split across deltas is escaped all the same.
	for _, delta := range []string{"hi \x1b", "[2Jthere\x07", "\n"} {
		r.render(agent.Event{Kind: agent.KindText, Agent: agent.DefaultAgentName, Text: delta})
	}

	if want := `hi \x1b[2Jthere\x07` + "\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}
