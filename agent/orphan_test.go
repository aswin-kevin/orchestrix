package agent

import (
	"encoding/json"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// asstUses returns an assistant message calling the tools with the given ids.
func asstUses(ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleAssistant}
	for _, id := range ids {
		m.Content = append(m.Content, llm.Block{Type: llm.BlockToolUse, ID: id, Name: "echo", Input: json.RawMessage(`{}`)})
	}
	return m
}

// userResults returns a user message answering the given ids, then text.
func userResults(text string, ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleUser}
	for _, id := range ids {
		m.Content = append(m.Content, llm.ToolResultText(id, "ok", false))
	}
	if text != "" {
		m.Content = append(m.Content, llm.TextBlock(text))
	}
	return m
}

func TestFindOrphans(t *testing.T) {
	msgs := []llm.Message{
		llm.UserText("hi"),
		asstUses("a", "b"),
		userResults("", "a"),
		asstUses("c"),
		llm.UserText("next prompt"),
		asstUses("d"),
	}
	sites := findOrphans(msgs)
	want := []struct {
		i  int
		id string
	}{{1, "b"}, {3, "c"}, {5, "d"}}
	if len(sites) != len(want) {
		t.Fatalf("sites = %v, want %d", sites, len(want))
	}
	for i, w := range want {
		if sites[i].assistant != w.i || sites[i].use.ID != w.id {
			t.Errorf("site %d = {%d %s}, want {%d %s}", i, sites[i].assistant, sites[i].use.ID, w.i, w.id)
		}
	}
}

func TestFindOrphansNone(t *testing.T) {
	msgs := []llm.Message{llm.UserText("hi"), asstUses("a"), userResults("", "a"), llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("done")}}}
	if got := findOrphans(msgs); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestPlaceResultsNewMessage(t *testing.T) {
	s := state.New()
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses("a")})
	r := &run{state: s}
	r.placeResults(map[int][]llm.Block{1: {llm.ToolResultText("a", "x", false)}})
	msgs := s.Messages()
	if len(msgs) != 3 || msgs[2].Role != llm.RoleUser || len(msgs[2].Content) != 1 ||
		msgs[2].Content[0].Type != llm.BlockToolResult || msgs[2].Content[0].ToolUseID != "a" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestPlaceResultsIntoExistingUserMessage(t *testing.T) {
	s := state.New()
	orig := userResults("go on", "a")
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses("a", "b"), orig})
	r := &run{state: s}
	r.placeResults(map[int][]llm.Block{1: {llm.ToolResultText("b", "x", false)}})
	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3", len(msgs))
	}
	c := msgs[2].Content
	if len(c) != 3 || c[0].ToolUseID != "a" || c[1].ToolUseID != "b" || c[2].Type != llm.BlockText || c[2].Text != "go on" {
		t.Fatalf("content = %+v", c)
	}
	if len(orig.Content) != 2 {
		t.Fatalf("input message mutated: %+v", orig.Content)
	}
}

func TestPlaceShiftsElisions(t *testing.T) {
	s := state.New()
	// Index 2 is an assistant message, so the result needs a new message
	// there, ahead of the elided range.
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses("a"), {Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("hm")}}, asstUses("x"), userResults("", "x")})
	if err := AddElision(s, Elision{ID: "e", From: 3, To: 5, Text: "gone"}); err != nil {
		t.Fatal(err)
	}
	r := &run{state: s}
	r.placeResults(map[int][]llm.Block{1: {llm.ToolResultText("a", "x", false)}})
	got, _ := ElisionsKey.Get(s)
	if len(s.Messages()) != 6 || len(got) != 1 || got[0].From != 4 || got[0].To != 6 {
		t.Fatalf("msgs = %d, elisions = %+v", len(s.Messages()), got)
	}
}
