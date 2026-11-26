package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
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

// validateView fails the test unless view starts with a user message and every
// tool_result answers a tool_use of the assistant message right before it.
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
		Request: llm.Request{Messages: elide.View(s, 0)},
		Profile: llm.Profile{ContextWindow: 100_000},
	}
	var got []llm.Message
	_, err := mustNew(t, cfg).WrapModel(context.Background(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
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
	boundaryKey.Set(s, Boundary{Index: 6, Summary: "S", Path: "/.orchestrix/conversation_history/t1/1.md"})

	got := wrapOnce(t, Config{}, s)

	wantSummary := "<summary>\nS\n</summary>\nEarlier conversation was summarized above. Its full transcript is in /.orchestrix/conversation_history/t1/1.md."
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
	boundaryKey.Set(s, Boundary{Index: 2, Summary: "S"})
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
	// A finished turn, so every result above has been seen by the model.
	msgs = append(msgs, assistantText("all done"), llm.UserText("thanks"))
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

func TestMicroCompactKeepsCurrentTurnResults(t *testing.T) {
	long := strings.Repeat("r", 3000)
	var ids []string
	for i := range 8 {
		ids = append(ids, fmt.Sprintf("p%d", i))
	}
	s := state.New()
	s.SetMessages([]llm.Message{llm.UserText("go"), assistantCalls(ids...), userResults(long, ids...)})

	got := wrapOnce(t, Config{}, s)

	for _, b := range got[2].Content {
		if b.Content[0].Text == ClearedResultText {
			t.Fatalf("result %s of the current turn was cleared before the model saw it", b.ToolUseID)
		}
	}
}

func TestOldToolInputTruncatedValidJSON(t *testing.T) {
	bigInput := fmt.Sprintf(`{"content":%q}`, strings.Repeat("é", 5000))
	old := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("w1", bigInput)}}
	loop1 := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("w2", bigInput)}}
	loop2 := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("w3", bigInput)}}
	s := state.New()
	// w2 and w3 belong to the tool loop answering the last plain user
	// message; providers may validate signed thinking across it.
	s.SetMessages([]llm.Message{
		llm.UserText("go"), old, userResults("ok", "w1"),
		llm.UserText("next"), loop1, userResults("ok", "w2"), loop2, userResults("ok", "w3"),
	})

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
	for _, i := range []int{4, 6} {
		if string(got[i].Content[0].Input) != bigInput {
			t.Fatalf("tool input of message %d, in the current tool loop, was truncated", i)
		}
	}
	if string(s.Messages()[1].Content[0].Input) != bigInput {
		t.Fatal("truncation changed State")
	}
}

func TestStaleBoundaryIgnored(t *testing.T) {
	s := state.New()
	s.SetMessages(mixedHistory()[:4])
	boundaryKey.Set(s, Boundary{Index: 9, Summary: "S", Compactions: 1})
	got := wrapOnce(t, Config{}, s)
	if len(got) != 4 || strings.Contains(got[0].Text(), "<summary>") {
		t.Fatalf("view = %d messages starting %q, want the 4 state messages", len(got), got[0].Text())
	}
}

func TestBoundaryCheckpointRoundTrip(t *testing.T) {
	s := state.New()
	s.SetMessages(mixedHistory()[:4])
	want := Boundary{Index: 2, Summary: "S", Path: HistoryDir + "/t1/1.md", Compactions: 1, LastError: "boom"}
	boundaryKey.Set(s, want)

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	restored := state.New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatal(err)
	}
	if got, ok := Current(restored); !ok || got != want {
		t.Fatalf("restored boundary = %+v (ok %v), want %+v", got, ok, want)
	}
}

func TestInjectedUserMessageIsNotATurn(t *testing.T) {
	bigInput := fmt.Sprintf(`{"content":%q}`, strings.Repeat("x", 5000))
	old := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("w1", bigInput)}}
	loop1 := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("w2", bigInput)}}
	loop2 := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{toolUse("w3", bigInput)}}
	// A middleware's notice in the middle of the tool loop, such as a
	// background job's end, does not start a new user turn.
	notice := llm.UserText("<task-notification>done</task-notification>")
	notice.Meta = map[string]string{agent.InjectedMetaKey: "tasks"}
	s := state.New()
	s.SetMessages([]llm.Message{
		llm.UserText("go"), old, userResults("ok", "w1"),
		llm.UserText("next"), loop1, userResults("ok", "w2"), notice, loop2, userResults("ok", "w3"),
	})

	got := wrapOnce(t, Config{}, s)

	if string(got[1].Content[0].Input) == bigInput {
		t.Fatal("the tool input before the last user turn was not shortened")
	}
	for _, i := range []int{4, 7} {
		if string(got[i].Content[0].Input) != bigInput {
			t.Fatalf("tool input of message %d, in the current tool loop, was shortened", i)
		}
	}
}

// continuing keeps a run going with one BeforeStop prompt per call.
type continuing struct{}

func (continuing) Name() string { return "continuing" }

func (continuing) BeforeStop(context.Context, *state.State, *llm.Response) (string, error) {
	return "check your work", nil
}

func TestStopContinuationIsNotATurn(t *testing.T) {
	big := strings.Repeat("x", 5000)
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w1", Name: "noop", Input: map[string]any{"content": big}}}},
		llmtest.Turn{Text: "first answer"},
		llmtest.Turn{Text: "second answer"},
		llmtest.Turn{Text: "third answer"},
		llmtest.Turn{Text: "fourth answer"},
	)
	a, err := agent.New(agent.Config{
		Provider:       p,
		Profile:        &llm.Profile{ContextWindow: 100_000, MaxOutput: 500},
		Tools:          []tool.Tool{noopTool()},
		Middleware:     []agent.Middleware{mustNew(t, Config{}), continuing{}},
		PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range a.Run(context.Background(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}) {
	}

	// The call answered the run's input; the continuations after it are
	// still the same turn, so its input is never shortened.
	for i, req := range p.Requests[2:] {
		if in := string(req.Messages[1].Content[0].Input); !strings.Contains(in, big) {
			t.Fatalf("request %d shortened the current run's tool input to %.80s", i+2, in)
		}
	}
}
