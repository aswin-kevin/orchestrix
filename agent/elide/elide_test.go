package elide_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/agent/internal/elision"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

func toolUse(id, input string) llm.Block {
	return llm.Block{Type: llm.BlockToolUse, ID: id, Name: "t", Input: json.RawMessage(input)}
}

func assistant(blocks ...llm.Block) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: blocks}
}

func user(blocks ...llm.Block) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: blocks}
}

// mustNew builds an agent or fails the test.
func mustNew(t *testing.T, cfg agent.Config) *agent.Agent {
	t.Helper()
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// collect drains a run and returns its final outcome, failing the test
// unless the run ends with KindFinal.
func collect(t *testing.T, seq iter.Seq2[agent.Event, error]) *agent.Final {
	t.Helper()
	var last agent.Event
	for ev := range seq {
		last = ev
	}
	if last.Kind != agent.KindFinal || last.Final == nil {
		t.Fatalf("last event = %+v, want KindFinal", last)
	}
	return last.Final
}

func resultText(b llm.Block) string {
	var parts []string
	for _, c := range b.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "")
}

type writeInput struct {
	Text string `json:"text"`
}

// writeTool is not read-only, so without rules the default mode asks for it.
func writeTool() tool.Tool {
	return tool.New("write_file", "Write.",
		func(_ context.Context, in writeInput, _ *tool.Call) (tool.Result, error) {
			return tool.Text("wrote " + in.Text), nil
		})
}

// assertHistoryPaired fails unless every tool_use in the history is answered
// by a tool_result in the message right after it.
func assertHistoryPaired(t *testing.T, msgs []llm.Message) {
	t.Helper()
	for i, m := range msgs {
		for _, use := range m.ToolUses() {
			answered := i+1 < len(msgs) && slices.ContainsFunc(msgs[i+1].Content, func(b llm.Block) bool {
				return b.Type == llm.BlockToolResult && b.ToolUseID == use.ID
			})
			if !answered {
				t.Fatalf("tool_use %q has no tool_result in the next message", use.ID)
			}
		}
	}
}

func userTexts(texts ...string) []llm.Message {
	msgs := make([]llm.Message, len(texts))
	for i, text := range texts {
		role := llm.RoleUser
		if text[0] == 'a' {
			role = llm.RoleAssistant
		}
		msgs[i] = llm.Message{Role: role, Content: []llm.Block{llm.TextBlock(text)}}
	}
	return msgs
}

// stateWith returns a State holding msgs.
func stateWith(msgs []llm.Message) *state.State {
	s := state.New()
	s.SetMessages(msgs)
	return s
}

func mustElide(t *testing.T, s *state.State, e elide.Elision) {
	t.Helper()
	if err := elide.Add(s, e); err != nil {
		t.Fatalf("Add(%+v): %v", e, err)
	}
}

// signed returns an assistant thinking block carrying a signature.
func signed(text string) llm.Block {
	return llm.Block{Type: llm.BlockThinking, Thinking: text, Signature: "sig-" + text}
}

// assertValidView fails unless view is a conversation providers accept: it
// starts with a user message, no two assistant messages are adjacent, every
// tool_use is answered in the next message, and every tool_result answers a
// tool_use of the message before it and leads its message.
func assertValidView(t *testing.T, view []llm.Message) {
	t.Helper()
	if len(view) == 0 || view[0].Role != llm.RoleUser {
		t.Fatalf("view must start with a user message: %+v", view)
	}
	assertHistoryPaired(t, view)
	for i, m := range view {
		if i > 0 && m.Role == llm.RoleAssistant && view[i-1].Role == llm.RoleAssistant {
			t.Fatalf("messages %d and %d are both assistant messages", i-1, i)
		}
		leading := true
		for _, b := range m.Content {
			if b.Type != llm.BlockToolResult {
				leading = false
				continue
			}
			if !leading {
				t.Fatalf("message %d: tool_result %s follows other content", i, b.ToolUseID)
			}
			if i == 0 || !slices.ContainsFunc(view[i-1].ToolUses(), func(u llm.Block) bool { return u.ID == b.ToolUseID }) {
				t.Fatalf("message %d: tool_result %s answers no call of the message before it", i, b.ToolUseID)
			}
		}
	}
}

// requestLen records how many State messages each model request was built
// from.
type requestLen struct{ n *[]int }

func (requestLen) Name() string { return "request_len" }

func (r requestLen) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	*r.n = append(*r.n, len(req.State.Messages()))
	return next(ctx, req)
}

func TestViewReplacesRange(t *testing.T) {
	s := stateWith(userTexts("u1", "a1", "u2", "a2"))
	mustElide(t, s, elide.Elision{ID: "c1", From: 0, To: 2, Text: "S"})
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	var lens []int
	a := mustNew(t, agent.Config{Provider: p, Middleware: []agent.Middleware{requestLen{&lens}}})

	final := collect(t, a.Run(t.Context(), agent.Input{State: s, Messages: userTexts("u3")}))

	want := []llm.Message{llm.UserText("S")}
	want = append(want, userTexts("u2", "a2", "u3")...)
	if got := p.Requests[0].Messages; !reflect.DeepEqual(got, want) {
		t.Fatalf("request messages = %+v, want %+v", got, want)
	}
	if !slices.Equal(lens, []int{5}) {
		t.Fatalf("State held %v messages at the request, want 5", lens)
	}
	if got := final.State.Messages(); len(got) != 6 || got[0].Text() != "u1" || got[1].Text() != "a1" {
		t.Fatalf("State = %+v, want the originals kept", got)
	}
}

// toolLoop is u0, a1(x), r2(x), a3(y,z), r4(y,z), a5, u6.
func toolLoop() []llm.Message {
	return []llm.Message{
		llm.UserText("u0"),
		assistant(signed("t1"), toolUse("x", `{}`)),
		user(llm.ToolResultText("x", "rx", false)),
		assistant(signed("t3"), toolUse("y", `{}`), toolUse("z", `{}`)),
		user(llm.ToolResultText("y", "ry", false), llm.ToolResultText("z", "rz", false)),
		assistant(signed("t5"), llm.TextBlock("a5")),
		llm.UserText("u6"),
	}
}

func TestAddValidation(t *testing.T) {
	existing := elide.Elision{ID: "c1", From: 1, To: 3, Text: "S1"}
	tests := []struct {
		name string
		e    elide.Elision
	}{
		{"overlap", elide.Elision{ID: "c2", From: 2, To: 5, Text: "S"}},
		{"contains existing", elide.Elision{ID: "c2", From: 0, To: 6, Text: "S"}},
		{"to beyond history", elide.Elision{ID: "c2", From: 5, To: 8, Text: "S"}},
		{"negative from", elide.Elision{ID: "c2", From: -1, To: 1, Text: "S"}},
		{"from equals to", elide.Elision{ID: "c2", From: 5, To: 5, Text: "S"}},
		{"from after to", elide.Elision{ID: "c2", From: 6, To: 5, Text: "S"}},
		{"duplicate id", elide.Elision{ID: "c1", From: 5, To: 7, Text: "S"}},
		{"empty id", elide.Elision{From: 5, To: 7, Text: "S"}},
		{"empty text", elide.Elision{ID: "c2", From: 5, To: 7}},
		{"ends between tool_use and tool_result", elide.Elision{ID: "c2", From: 3, To: 4, Text: "S"}},
		{"starts at a tool_result", elide.Elision{ID: "c2", From: 4, To: 6, Text: "S"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := stateWith(toolLoop())
			mustElide(t, s, existing)
			err := elide.Add(s, tt.e)
			if !errors.Is(err, elide.ErrInvalid) {
				t.Fatalf("Add(%+v) = %v, want ErrInvalid", tt.e, err)
			}
			if got := elide.All(s); !reflect.DeepEqual(got, []elide.Elision{existing}) {
				t.Fatalf("key = %+v after a refused elision, want only %+v", got, existing)
			}
		})
	}
	s := stateWith(toolLoop())
	if err := elide.Add(s, elide.Elision{ID: "c1", From: 3, To: 4, Text: "S"}); !errors.Is(err, elide.ErrInvalid) {
		t.Fatalf("Add on an empty key = %v, want ErrInvalid", err)
	}
	if _, ok := elision.Key.Get(s); ok {
		t.Fatal("a refused elision set the key")
	}
}

func TestAddRefusesUnansweredCall(t *testing.T) {
	// The interrupted turn's results are inserted after a1 on resume, so
	// no elision may hold it.
	s := stateWith([]llm.Message{llm.UserText("u0"), assistant(toolUse("x", `{}`))})
	if err := elide.Add(s, elide.Elision{ID: "c1", From: 0, To: 2, Text: "S"}); !errors.Is(err, elide.ErrInvalid) {
		t.Fatalf("Add over an unanswered call = %v, want ErrInvalid", err)
	}
}

func TestAddKeepsKeySorted(t *testing.T) {
	s := stateWith(toolLoop())
	mustElide(t, s, elide.Elision{ID: "late", From: 5, To: 7, Text: "L"})
	mustElide(t, s, elide.Elision{ID: "early", From: 0, To: 3, Text: "E"})
	mustElide(t, s, elide.Elision{ID: "mid", From: 3, To: 5, Text: "M"})
	got := elide.All(s)
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []string{"early", "mid", "late"}) {
		t.Fatalf("key order = %v, want sorted by From", ids)
	}
	if view := elide.View(s, 0); !reflect.DeepEqual(view, []llm.Message{llm.UserText("E"), llm.UserText("M"), llm.UserText("L")}) {
		t.Fatalf("view = %+v, want the three placeholders", view)
	}
}

func TestElisionBeyondHistoryIgnored(t *testing.T) {
	msgs := userTexts("u0", "a1", "u2", "a3", "u4")
	s := stateWith(msgs)
	elision.Key.Set(s, []elision.Elision{{ID: "c1", From: 2, To: 9, Text: "S"}})
	if got := elide.View(s, 0); !reflect.DeepEqual(got, agent.View(msgs)) {
		t.Fatalf("view = %+v, want View(messages)", got)
	}
}

func TestElisionStraddlingFromIgnored(t *testing.T) {
	msgs := userTexts("u0", "a1", "u2", "a3", "u4")
	s := stateWith(msgs)
	mustElide(t, s, elide.Elision{ID: "c1", From: 1, To: 4, Text: "S"})
	if got := elide.View(s, 2); !reflect.DeepEqual(got, agent.View(msgs[2:])) {
		t.Fatalf("View(s, 2) = %+v, want View(messages[2:])", got)
	}
	if got := elide.View(s, 1); !reflect.DeepEqual(got, []llm.Message{llm.UserText("S"), msgs[4]}) {
		t.Fatalf("View(s, 1) = %+v, want the placeholder and u4", got)
	}
	if got := elide.View(s, -3); !reflect.DeepEqual(got, elide.View(s, 0)) {
		t.Fatal("a negative from is not clamped to 0")
	}
	if got := elide.View(s, 99); len(got) != 0 {
		t.Fatalf("View past the end = %+v, want empty", got)
	}
}

// pairedHistories are tool-loop histories, with signed thinking, an
// injected notice between two user messages and a dangling call repaired
// in the view.
func pairedHistories() map[string][]llm.Message {
	notice := llm.UserText("notice")
	notice.Meta = map[string]string{agent.InjectedMetaKey: "test"}
	return map[string][]llm.Message{
		"tool loop": toolLoop(),
		"loop then notice": {
			llm.UserText("u0"),
			assistant(signed("t1"), toolUse("x", `{}`)),
			user(llm.ToolResultText("x", "rx", false)),
			notice,
			assistant(signed("t4"), toolUse("y", `{"big":1}`)),
			user(llm.ToolResultText("y", "ry", false)),
			assistant(llm.TextBlock("a6")),
		},
		"dangling call": {
			llm.UserText("u0"),
			assistant(toolUse("old", `{}`)),
			llm.UserText("u2"),
			assistant(toolUse("x", `{}`)),
			user(llm.ToolResultText("x", "rx", false)),
			assistant(llm.TextBlock("a5")),
		},
	}
}

func TestViewAlwaysPaired(t *testing.T) {
	for name, msgs := range pairedHistories() {
		t.Run(name, func(t *testing.T) {
			accepted, refused := 0, 0
			for from := 0; from < len(msgs); from++ {
				for to := from + 1; to <= len(msgs); to++ {
					s := stateWith(msgs)
					if err := elide.Add(s, elide.Elision{ID: "c1", From: from, To: to, Text: "S"}); err != nil {
						refused++
						continue
					}
					accepted++
					checkElidedView(t, s, msgs)
					// A second elision after the first, wherever one fits.
					for from2 := to; from2 < len(msgs); from2++ {
						for to2 := from2 + 1; to2 <= len(msgs); to2++ {
							s2 := stateWith(msgs)
							mustElide(t, s2, elide.Elision{ID: "c1", From: from, To: to, Text: "S"})
							if elide.Add(s2, elide.Elision{ID: "c2", From: from2, To: to2, Text: "T"}) == nil {
								checkElidedView(t, s2, msgs)
							}
						}
					}
				}
			}
			if accepted == 0 || refused == 0 {
				t.Fatalf("accepted %d and refused %d elisions, want some of each", accepted, refused)
			}
		})
	}
}

// checkElidedView requires the elided view of s to be valid, and every
// assistant message in it to be the original, signed thinking included.
func checkElidedView(t *testing.T, s *state.State, msgs []llm.Message) {
	t.Helper()
	view := elide.View(s, 0)
	assertValidView(t, view)
	originals := agent.View(msgs)
	for _, m := range view {
		if m.Role == llm.RoleAssistant && !slices.ContainsFunc(originals, func(o llm.Message) bool { return reflect.DeepEqual(o, m) }) {
			els := elide.All(s)
			t.Fatalf("elisions %+v changed assistant message %+v", els, m)
		}
	}
}

func TestViewHostReplacedState(t *testing.T) {
	s := stateWith(userTexts("u0", "a1", "u2", "a3", "u4", "a5"))
	mustElide(t, s, elide.Elision{ID: "c1", From: 1, To: 3, Text: "S"})
	// The host swaps in a different history without clearing the key: the
	// range [1, 3) would now end between a tool_use and its tool_result.
	replaced := []llm.Message{
		llm.UserText("u0"), assistant(llm.TextBlock("a1")),
		assistant(toolUse("x", `{}`)), user(llm.ToolResultText("x", "rx", false)),
		assistant(llm.TextBlock("a4")),
	}
	s.SetMessages(replaced)
	view := elide.View(s, 0)
	if !reflect.DeepEqual(view, agent.View(replaced)) {
		t.Fatalf("view = %+v, want the elision ignored", view)
	}
	assertHistoryPaired(t, view)
}

func TestViewOverlappingKeyEntries(t *testing.T) {
	msgs := userTexts("u0", "a1", "u2", "a3", "u4", "a5")
	s := stateWith(msgs)
	// Set directly, bypassing Add: unsorted, overlapping, duplicated.
	elision.Key.Set(s, []elision.Elision{
		{ID: "c3", From: 4, To: 6, Text: "C"},
		{ID: "c1", From: 0, To: 2, Text: "A"},
		{ID: "c2", From: 1, To: 4, Text: "B"},
		{ID: "c1", From: 0, To: 2, Text: "A"},
		{ID: "c4", From: 3, To: 3, Text: "D"},
		{ID: "c5", From: 2, To: 3},
	})
	want := []llm.Message{llm.UserText("A"), msgs[2], msgs[3], llm.UserText("C")}
	if got := elide.View(s, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("view = %+v, want %+v", got, want)
	}
	applied := elide.Applied(s, 0)
	if len(applied) != 2 || applied[0].ID != "c1" || applied[1].ID != "c3" {
		t.Fatalf("Applied = %+v, want c1 and c3", applied)
	}
}

func TestRemove(t *testing.T) {
	msgs := userTexts("u0", "a1", "u2", "a3", "u4")
	s := stateWith(msgs)
	mustElide(t, s, elide.Elision{ID: "c1", From: 0, To: 2, Text: "S"})
	mustElide(t, s, elide.Elision{ID: "c2", From: 2, To: 4, Text: "T"})
	if !elide.Remove(s, "c1") {
		t.Fatal("Remove(c1) = false")
	}
	if elide.Remove(s, "c1") {
		t.Fatal("Remove(c1) twice = true")
	}
	if got := elide.View(s, 0); !reflect.DeepEqual(got, []llm.Message{msgs[0], msgs[1], llm.UserText("T"), msgs[4]}) {
		t.Fatalf("view after removing c1 = %+v", got)
	}
	if !elide.Remove(s, "c2") {
		t.Fatal("Remove(c2) = false")
	}
	if _, ok := elision.Key.Get(s); ok {
		t.Fatal("key still set with no elisions left")
	}
	if elide.Remove(state.New(), "c1") {
		t.Fatal("Remove on a fresh State = true")
	}
	// The ID is free again.
	mustElide(t, s, elide.Elision{ID: "c1", From: 1, To: 3, Text: "S"})
}

func TestElisionSurvivesCheckpoint(t *testing.T) {
	s := stateWith(userTexts("u0", "a1", "u2", "a3", "u4"))
	mustElide(t, s, elide.Elision{ID: "c1", From: 0, To: 2, Text: "S"})
	want := elide.All(s)

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	restored := state.New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatal(err)
	}
	if got := elide.All(restored); !reflect.DeepEqual(got, want) {
		t.Fatalf("after JSON = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(elide.View(restored, 0), elide.View(s, 0)) {
		t.Fatal("restored State has a different view")
	}
	if got := elide.All(s.Fork()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Fork = %+v, want %+v", got, want)
	}
	if _, ok := elision.Key.Get(s.Child()); ok {
		t.Fatal("Child copied the elisions")
	}
}

// elideNoteOnResume is a BeforeAgent hook that, once on is set, appends a
// note and elides it.
type elideNoteOnResume struct{ on *atomic.Bool }

func (elideNoteOnResume) Name() string { return "elide_note_on_resume" }

func (h elideNoteOnResume) BeforeAgent(_ context.Context, s *state.State) error {
	if !h.on.Load() {
		return nil
	}
	s.AppendMessages(llm.UserText("hook note"))
	n := len(s.Messages())
	return elide.Add(s, elide.Elision{ID: "note", From: n - 1, To: n, Text: "E"})
}

func TestElisionShiftedWhenResumeInserts(t *testing.T) {
	var on atomic.Bool
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w", Name: "write_file", Input: map[string]any{"text": "w"}}}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, agent.Config{Provider: p, Tools: []tool.Tool{writeTool()}, Middleware: []agent.Middleware{elideNoteOnResume{&on}}})
	final := collect(t, a.Run(t.Context(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}))
	if final.Reason != agent.ReasonInterrupted {
		t.Fatalf("final = %+v, want interrupted", final)
	}

	on.Store(true)
	resumed := collect(t, a.Resume(t.Context(), "", final.State, []agent.Decision{{ToolUseID: "w", Type: agent.Approve}}))
	if resumed.Reason != agent.ReasonCompleted || resumed.Err != nil || resumed.Text != "done" {
		t.Fatalf("resumed final = %+v, want completed with text", resumed)
	}

	if got := elide.All(final.State); len(got) != 1 || got[0].From != 3 || got[0].To != 4 {
		t.Fatalf("elisions = %+v, want the note's [3, 4) after the inserted results", got)
	}
	sent := p.Requests[1].Messages
	assertValidView(t, sent)
	if len(sent) != 4 || sent[3].Text() != "E" {
		t.Fatalf("request = %+v, want user, assistant, results, placeholder", sent)
	}
	if r := sent[2].Content[0]; r.ToolUseID != "w" || r.IsError || resultText(r) != "wrote w" {
		t.Fatalf("message after the tool_use = %+v, want w's real result", sent[2])
	}
}

func TestCheck(t *testing.T) {
	msgs := []llm.Message{
		llm.UserText("go"),
		{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockToolUse, ID: "a", Name: "echo"}}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.ToolResultText("a", "ok", false)}},
	}
	if err := elide.Check(msgs, elide.Elision{ID: "e", Text: "t", From: 0, To: 3}); err != nil {
		t.Fatalf("self-contained range refused: %v", err)
	}
	for name, e := range map[string]elide.Elision{
		"unanswered call": {ID: "e", Text: "t", From: 0, To: 2},
		"orphan result":   {ID: "e", Text: "t", From: 2, To: 3},
		"empty id":        {Text: "t", From: 0, To: 1},
		"out of range":    {ID: "e", Text: "t", From: 0, To: 4},
	} {
		if err := elide.Check(msgs, e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAllAndClear(t *testing.T) {
	msgs := userTexts("u0", "a1", "u2", "a3", "u4")
	s := stateWith(msgs)
	mustElide(t, s, elide.Elision{ID: "c2", From: 2, To: 4, Text: "T"})
	mustElide(t, s, elide.Elision{ID: "c1", From: 0, To: 2, Text: "S"})
	all := elide.All(s)
	if len(all) != 2 {
		t.Fatalf("All = %+v, want 2 elisions", all)
	}
	// All returns a copy: changing it leaves State alone.
	all[0].Text = "changed"
	if got := elide.All(s); got[0].Text == "changed" {
		t.Fatal("All returned the stored slice, not a copy")
	}
	elide.Clear(s)
	if got := elide.All(s); len(got) != 0 {
		t.Fatalf("All after Clear = %+v, want empty", got)
	}
	if got := elide.View(s, 0); !reflect.DeepEqual(got, agent.View(msgs)) {
		t.Fatalf("View after Clear = %+v, want the unelided messages", got)
	}
}

func TestErrInvalid(t *testing.T) {
	s := stateWith(userTexts("u0", "a1", "u2", "a3", "u4"))
	mustElide(t, s, elide.Elision{ID: "c1", From: 0, To: 3, Text: "S"})
	err := elide.Add(s, elide.Elision{ID: "c2", From: 2, To: 4, Text: "T"})
	if !errors.Is(err, elide.ErrInvalid) {
		t.Fatalf("Add(overlapping) = %v, want ErrInvalid", err)
	}
	if got := elide.ErrInvalid.Error(); got != "elide: invalid elision" {
		t.Fatalf("ErrInvalid = %q", got)
	}
}
