package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

func assistantText(s string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock(s)}}
}

func toolUse(id string, input string) llm.Block {
	return llm.Block{Type: llm.BlockToolUse, ID: id, Name: "run", Input: json.RawMessage(input)}
}

func assistantCalls(ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleAssistant}
	for _, id := range ids {
		m.Content = append(m.Content, toolUse(id, `{"cmd":"ls"}`))
	}
	return m
}

func userResults(text string, ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleUser}
	for _, id := range ids {
		m.Content = append(m.Content, llm.ToolResultText(id, text, false))
	}
	return m
}

// mixedHistory returns 40 messages mixing plain text turns with single and
// parallel tool turns.
func mixedHistory() []llm.Message {
	var msgs []llm.Message
	for i := 0; len(msgs) < 40; i++ {
		switch i % 3 {
		case 0:
			msgs = append(msgs, llm.UserText(fmt.Sprintf("question %d", i)), assistantText(fmt.Sprintf("answer %d", i)))
		case 1:
			id := fmt.Sprintf("s%d", i)
			msgs = append(msgs, assistantCalls(id), userResults("ok", id))
		default:
			a, b := fmt.Sprintf("p%da", i), fmt.Sprintf("p%db", i)
			msgs = append(msgs, assistantCalls(a, b), userResults("ok", a, b))
		}
	}
	return msgs[:40]
}

func holdsToolResult(m llm.Message) bool {
	return m.Role == llm.RoleUser && slices.ContainsFunc(m.Content, func(b llm.Block) bool { return b.Type == llm.BlockToolResult })
}

// validateView fails the test unless view starts with a user message and
// every tool_result answers a tool_use of the assistant message right before it.
func validateView(t *testing.T, view []llm.Message) {
	t.Helper()
	if len(view) == 0 || view[0].Role != llm.RoleUser {
		t.Fatalf("view must start with a user message: %+v", view)
	}
	for i, m := range view {
		for _, b := range m.Content {
			if b.Type != llm.BlockToolResult {
				continue
			}
			if i == 0 || view[i-1].Role != llm.RoleAssistant ||
				!slices.ContainsFunc(view[i-1].ToolUses(), func(u llm.Block) bool { return u.ID == b.ToolUseID }) {
				t.Fatalf("message %d: tool_result %s does not answer the preceding assistant message", i, b.ToolUseID)
			}
		}
	}
}

func TestCutNeverSplitsToolPair(t *testing.T) {
	msgs := mixedHistory()
	for from := 0; from < len(msgs); from++ {
		for want := from + 1; want <= len(msgs); want++ {
			k := SafeCut(msgs, from, want)
			if k < from || k > want {
				t.Fatalf("SafeCut(from=%d, want=%d) = %d, outside [from, want]", from, want, k)
			}
			if k < len(msgs) && holdsToolResult(msgs[k]) {
				t.Fatalf("SafeCut(from=%d, want=%d) = %d starts at a tool_result message", from, want, k)
			}
			for j := k + 1; j <= want; j++ {
				if j == len(msgs) || !holdsToolResult(msgs[j]) {
					t.Fatalf("SafeCut(from=%d, want=%d) = %d, but %d is a larger safe cut", from, want, k, j)
				}
			}
			if k == from {
				continue
			}
			view := append([]llm.Message{llm.UserText("<summary>s</summary>")}, agent.View(msgs[k:])...)
			validateView(t, view)
		}
	}
}

// wrapOnce runs the middleware's WrapModel over the loop's request view of s
// and returns the messages the next handler received.
func wrapOnce(t *testing.T, cfg Config, s *state.State) []llm.Message {
	t.Helper()
	req := &agent.ModelRequest{
		State:   s,
		Request: llm.Request{Messages: agent.View(s.Messages())},
		Profile: llm.Profile{ContextWindow: 100_000},
	}
	var got []llm.Message
	_, err := New(cfg).(agent.ModelWrapper).WrapModel(context.Background(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
		got = r.Request.Messages
		return &llm.Response{Message: assistantText("ok")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestViewUsesBoundaryStateUntouched(t *testing.T) {
	s := state.New()
	s.SetMessages(mixedHistory()[:10])
	before := s.Messages()
	BoundaryKey.Set(s, Boundary{Index: 6, Summary: "S", Path: "/conversation_history/t1/1.md"})

	got := wrapOnce(t, Config{}, s)

	wantSummary := "<summary>\nS\n</summary>\nEarlier conversation was summarized above. Its full transcript is in /conversation_history/t1/1.md."
	if len(got) != 5 || got[0].Role != llm.RoleUser || got[0].Text() != wantSummary {
		t.Fatalf("view = %d messages starting %q, want summary + 4 messages", len(got), got[0].Text())
	}
	for i, m := range got[1:] {
		if m.Role != before[6+i].Role || m.Text() != before[6+i].Text() || len(m.Content) != len(before[6+i].Content) {
			t.Fatalf("view message %d = %+v, want state message %d", i+1, m, 6+i)
		}
	}
	validateView(t, got)
	if len(s.Messages()) != 10 {
		t.Fatalf("state has %d messages, want 10", len(s.Messages()))
	}
}

func TestViewWithoutPathOmitsTranscriptPointer(t *testing.T) {
	s := state.New()
	s.SetMessages(mixedHistory()[:4])
	BoundaryKey.Set(s, Boundary{Index: 2, Summary: "S"})
	got := wrapOnce(t, Config{}, s)
	if want := "<summary>\nS\n</summary>\nEarlier conversation was summarized above."; got[0].Text() != want {
		t.Fatalf("summary message = %q, want %q", got[0].Text(), want)
	}
}

func TestMicroCompactClearsOldResultsOnly(t *testing.T) {
	long := strings.Repeat("r", 3000)
	msgs := []llm.Message{llm.UserText("start"), assistantCalls("short1", "short2"), userResults("tiny", "short1", "short2")}
	for i := range 10 {
		id := fmt.Sprintf("long%d", i)
		msgs = append(msgs, assistantCalls(id), userResults(long, id))
	}
	s := state.New()
	s.SetMessages(msgs)

	got := wrapOnce(t, Config{}, s)

	validateView(t, got)
	results := map[string]string{}
	for _, m := range got {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				results[b.ToolUseID] = b.Content[0].Text
			}
		}
	}
	for _, id := range []string{"short1", "short2"} {
		if results[id] != "tiny" {
			t.Errorf("short result %s changed to %q", id, results[id])
		}
	}
	for i := range 10 {
		id := fmt.Sprintf("long%d", i)
		cleared := results[id] == ClearedResultText
		if wantCleared := i < 4; cleared != wantCleared {
			t.Errorf("%s cleared = %v, want %v", id, cleared, wantCleared)
		}
	}
	for _, m := range s.Messages() {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && b.Content[0].Text == ClearedResultText {
				t.Fatal("micro-compaction changed State")
			}
		}
	}
}

func TestOldToolInputTruncatedValidJSON(t *testing.T) {
	bigInput := fmt.Sprintf(`{"content":%q}`, strings.Repeat("é", 5000))
	old := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("w1", bigInput)}}
	last := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("w2", bigInput)}}
	s := state.New()
	s.SetMessages([]llm.Message{llm.UserText("go"), old, userResults("ok", "w1"), last, userResults("ok", "w2")})

	got := wrapOnce(t, Config{}, s)

	oldInput := got[1].Content[0].Input
	if !json.Valid(oldInput) {
		t.Fatalf("truncated input is not valid JSON: %.100s", oldInput)
	}
	var parsed map[string]string
	if err := json.Unmarshal(oldInput, &parsed); err != nil {
		t.Fatal(err)
	}
	prefix := string([]rune(bigInput)[:500])
	if len(parsed) != 1 || parsed["truncated"] != prefix+"…" {
		t.Fatalf("truncated input = %.120s, want {\"truncated\": first 500 chars + …}", oldInput)
	}
	if string(got[3].Content[0].Input) != bigInput {
		t.Fatal("the last assistant message's tool input was truncated")
	}
	if string(s.Messages()[1].Content[0].Input) != bigInput {
		t.Fatal("truncation changed State")
	}
}
