package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

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

func mustElide(t *testing.T, s *state.State, e Elision) {
	t.Helper()
	if err := AddElision(s, e); err != nil {
		t.Fatalf("AddElision(%+v): %v", e, err)
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

func (r requestLen) WrapModel(ctx context.Context, req *ModelRequest, next ModelHandler) (*llm.Response, error) {
	*r.n = append(*r.n, len(req.State.Messages()))
	return next(ctx, req)
}

func TestElidedViewReplacesRange(t *testing.T) {
	s := stateWith(userTexts("u1", "a1", "u2", "a2"))
	mustElide(t, s, Elision{ID: "c1", From: 0, To: 2, Text: "S"})
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	var lens []int
	a := mustNew(t, Config{Provider: p, Middleware: []Middleware{requestLen{&lens}}})

	_, final := collect(t, a.Run(context.Background(), Input{State: s, Messages: userTexts("u3")}))

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

func TestAddElisionValidation(t *testing.T) {
	existing := Elision{ID: "c1", From: 1, To: 3, Text: "S1"}
	cases := []struct {
		name string
		e    Elision
	}{
		{"overlap", Elision{ID: "c2", From: 2, To: 5, Text: "S"}},
		{"contains existing", Elision{ID: "c2", From: 0, To: 6, Text: "S"}},
		{"to beyond history", Elision{ID: "c2", From: 5, To: 8, Text: "S"}},
		{"negative from", Elision{ID: "c2", From: -1, To: 1, Text: "S"}},
		{"from equals to", Elision{ID: "c2", From: 5, To: 5, Text: "S"}},
		{"from after to", Elision{ID: "c2", From: 6, To: 5, Text: "S"}},
		{"duplicate id", Elision{ID: "c1", From: 5, To: 7, Text: "S"}},
		{"empty id", Elision{From: 5, To: 7, Text: "S"}},
		{"empty text", Elision{ID: "c2", From: 5, To: 7}},
		{"ends between tool_use and tool_result", Elision{ID: "c2", From: 3, To: 4, Text: "S"}},
		{"starts at a tool_result", Elision{ID: "c2", From: 4, To: 6, Text: "S"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := stateWith(toolLoop())
			mustElide(t, s, existing)
			err := AddElision(s, tc.e)
			if !errors.Is(err, ErrInvalidElision) {
				t.Fatalf("AddElision(%+v) = %v, want ErrInvalidElision", tc.e, err)
			}
			if got, _ := ElisionsKey.Get(s); !reflect.DeepEqual(got, []Elision{existing}) {
				t.Fatalf("key = %+v after a refused elision, want only %+v", got, existing)
			}
		})
	}
	s := stateWith(toolLoop())
	if err := AddElision(s, Elision{ID: "c1", From: 3, To: 4, Text: "S"}); !errors.Is(err, ErrInvalidElision) {
		t.Fatalf("AddElision on an empty key = %v, want ErrInvalidElision", err)
	}
	if _, ok := ElisionsKey.Get(s); ok {
		t.Fatal("a refused elision set the key")
	}
}

func TestAddElisionRefusesUnansweredCall(t *testing.T) {
	// The interrupted turn's results are inserted after a1 on resume, so
	// no elision may hold it.
	s := stateWith([]llm.Message{llm.UserText("u0"), assistant(toolUse("x", `{}`))})
	if err := AddElision(s, Elision{ID: "c1", From: 0, To: 2, Text: "S"}); !errors.Is(err, ErrInvalidElision) {
		t.Fatalf("AddElision over an unanswered call = %v, want ErrInvalidElision", err)
	}
}

func TestAddElisionKeepsKeySorted(t *testing.T) {
	s := stateWith(toolLoop())
	mustElide(t, s, Elision{ID: "late", From: 5, To: 7, Text: "L"})
	mustElide(t, s, Elision{ID: "early", From: 0, To: 3, Text: "E"})
	mustElide(t, s, Elision{ID: "mid", From: 3, To: 5, Text: "M"})
	got, _ := ElisionsKey.Get(s)
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []string{"early", "mid", "late"}) {
		t.Fatalf("key order = %v, want sorted by From", ids)
	}
	if view := ElidedView(s, 0); !reflect.DeepEqual(view, []llm.Message{llm.UserText("E"), llm.UserText("M"), llm.UserText("L")}) {
		t.Fatalf("view = %+v, want the three placeholders", view)
	}
}

func TestElisionBeyondHistoryIgnored(t *testing.T) {
	msgs := userTexts("u0", "a1", "u2", "a3", "u4")
	s := stateWith(msgs)
	ElisionsKey.Set(s, []Elision{{ID: "c1", From: 2, To: 9, Text: "S"}})
	if got := ElidedView(s, 0); !reflect.DeepEqual(got, View(msgs)) {
		t.Fatalf("view = %+v, want View(messages)", got)
	}
}

func TestElisionStraddlingFromIgnored(t *testing.T) {
	msgs := userTexts("u0", "a1", "u2", "a3", "u4")
	s := stateWith(msgs)
	mustElide(t, s, Elision{ID: "c1", From: 1, To: 4, Text: "S"})
	if got := ElidedView(s, 2); !reflect.DeepEqual(got, View(msgs[2:])) {
		t.Fatalf("ElidedView(s, 2) = %+v, want View(messages[2:])", got)
	}
	if got := ElidedView(s, 1); !reflect.DeepEqual(got, []llm.Message{llm.UserText("S"), msgs[4]}) {
		t.Fatalf("ElidedView(s, 1) = %+v, want the placeholder and u4", got)
	}
	if got := ElidedView(s, -3); !reflect.DeepEqual(got, ElidedView(s, 0)) {
		t.Fatal("a negative from is not clamped to 0")
	}
	if got := ElidedView(s, 99); len(got) != 0 {
		t.Fatalf("ElidedView past the end = %+v, want empty", got)
	}
}

// pairedHistories are tool-loop histories, with signed thinking, an
// injected notice between two user messages and a dangling call repaired
// in the view.
func pairedHistories() map[string][]llm.Message {
	notice := llm.UserText("notice")
	notice.Meta = map[string]string{InjectedMetaKey: "test"}
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

func TestElidedViewAlwaysPaired(t *testing.T) {
	for name, msgs := range pairedHistories() {
		t.Run(name, func(t *testing.T) {
			accepted, refused := 0, 0
			for from := 0; from < len(msgs); from++ {
				for to := from + 1; to <= len(msgs); to++ {
					s := stateWith(msgs)
					if err := AddElision(s, Elision{ID: "c1", From: from, To: to, Text: "S"}); err != nil {
						refused++
						continue
					}
					accepted++
					checkElidedView(t, s, msgs)
					// A second elision after the first, wherever one fits.
					for from2 := to; from2 < len(msgs); from2++ {
						for to2 := from2 + 1; to2 <= len(msgs); to2++ {
							s2 := stateWith(msgs)
							mustElide(t, s2, Elision{ID: "c1", From: from, To: to, Text: "S"})
							if AddElision(s2, Elision{ID: "c2", From: from2, To: to2, Text: "T"}) == nil {
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
	view := ElidedView(s, 0)
	assertValidView(t, view)
	originals := View(msgs)
	for _, m := range view {
		if m.Role == llm.RoleAssistant && !slices.ContainsFunc(originals, func(o llm.Message) bool { return reflect.DeepEqual(o, m) }) {
			els, _ := ElisionsKey.Get(s)
			t.Fatalf("elisions %+v changed assistant message %+v", els, m)
		}
	}
}

func TestElidedViewHostReplacedState(t *testing.T) {
	s := stateWith(userTexts("u0", "a1", "u2", "a3", "u4", "a5"))
	mustElide(t, s, Elision{ID: "c1", From: 1, To: 3, Text: "S"})
	// The host swaps in a different history without clearing the key: the
	// range [1, 3) would now end between a tool_use and its tool_result.
	replaced := []llm.Message{
		llm.UserText("u0"), assistant(llm.TextBlock("a1")),
		assistant(toolUse("x", `{}`)), user(llm.ToolResultText("x", "rx", false)),
		assistant(llm.TextBlock("a4")),
	}
	s.SetMessages(replaced)
	view := ElidedView(s, 0)
	if !reflect.DeepEqual(view, View(replaced)) {
		t.Fatalf("view = %+v, want the elision ignored", view)
	}
	assertHistoryPaired(t, view)
}

func TestElidedViewOverlappingKeyEntries(t *testing.T) {
	msgs := userTexts("u0", "a1", "u2", "a3", "u4", "a5")
	s := stateWith(msgs)
	// Set directly, bypassing AddElision: unsorted, overlapping, duplicated.
	ElisionsKey.Set(s, []Elision{
		{ID: "c3", From: 4, To: 6, Text: "C"},
		{ID: "c1", From: 0, To: 2, Text: "A"},
		{ID: "c2", From: 1, To: 4, Text: "B"},
		{ID: "c1", From: 0, To: 2, Text: "A"},
		{ID: "c4", From: 3, To: 3, Text: "D"},
		{ID: "c5", From: 2, To: 3},
	})
	want := []llm.Message{llm.UserText("A"), msgs[2], msgs[3], llm.UserText("C")}
	if got := ElidedView(s, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("view = %+v, want %+v", got, want)
	}
	applied := AppliedElisions(s, 0)
	if len(applied) != 2 || applied[0].ID != "c1" || applied[1].ID != "c3" {
		t.Fatalf("AppliedElisions = %+v, want c1 and c3", applied)
	}
}

func TestRemoveElision(t *testing.T) {
	msgs := userTexts("u0", "a1", "u2", "a3", "u4")
	s := stateWith(msgs)
	mustElide(t, s, Elision{ID: "c1", From: 0, To: 2, Text: "S"})
	mustElide(t, s, Elision{ID: "c2", From: 2, To: 4, Text: "T"})
	if !RemoveElision(s, "c1") {
		t.Fatal("RemoveElision(c1) = false")
	}
	if RemoveElision(s, "c1") {
		t.Fatal("RemoveElision(c1) twice = true")
	}
	if got := ElidedView(s, 0); !reflect.DeepEqual(got, []llm.Message{msgs[0], msgs[1], llm.UserText("T"), msgs[4]}) {
		t.Fatalf("view after removing c1 = %+v", got)
	}
	if !RemoveElision(s, "c2") {
		t.Fatal("RemoveElision(c2) = false")
	}
	if _, ok := ElisionsKey.Get(s); ok {
		t.Fatal("key still set with no elisions left")
	}
	if RemoveElision(state.New(), "c1") {
		t.Fatal("RemoveElision on a fresh State = true")
	}
	// The ID is free again.
	mustElide(t, s, Elision{ID: "c1", From: 1, To: 3, Text: "S"})
}

func TestElisionSurvivesCheckpoint(t *testing.T) {
	s := stateWith(userTexts("u0", "a1", "u2", "a3", "u4"))
	mustElide(t, s, Elision{ID: "c1", From: 0, To: 2, Text: "S"})
	want, _ := ElisionsKey.Get(s)

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	restored := state.New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatal(err)
	}
	if got, _ := ElisionsKey.Get(restored); !reflect.DeepEqual(got, want) {
		t.Fatalf("after JSON = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(ElidedView(restored, 0), ElidedView(s, 0)) {
		t.Fatal("restored State has a different view")
	}
	if got, _ := ElisionsKey.Get(s.Fork()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Fork = %+v, want %+v", got, want)
	}
	if _, ok := ElisionsKey.Get(s.ChildFor()); ok {
		t.Fatal("ChildFor copied the elisions")
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
	return AddElision(s, Elision{ID: "note", From: n - 1, To: n, Text: "E"})
}

func TestElisionShiftedWhenResumeInserts(t *testing.T) {
	log := newCallLog()
	var on atomic.Bool
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w")}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{writeTool(log)}, Middleware: []Middleware{elideNoteOnResume{&on}}})
	_, final := collect(t, a.Run(context.Background(), userInput("go")))
	if final.Reason != ReasonInterrupted {
		t.Fatalf("final = %+v, want interrupted", final)
	}

	on.Store(true)
	resumeCompleted(t, a, final.State, Decision{ToolUseID: "w", Type: Approve})

	if got, _ := ElisionsKey.Get(final.State); len(got) != 1 || got[0].From != 3 || got[0].To != 4 {
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
