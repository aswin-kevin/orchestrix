package summarize

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
)

// texts returns a plain conversation: messages starting with 'a' are the
// assistant's, the rest the user's.
func texts(ts ...string) []llm.Message {
	msgs := make([]llm.Message, len(ts))
	for i, text := range ts {
		msgs[i] = llm.UserText(text)
		if text[0] == 'a' {
			msgs[i] = assistantText(text)
		}
	}
	return msgs
}

func mustElide(t *testing.T, s *state.State, e elide.Elision) {
	t.Helper()
	if err := elide.Add(s, e); err != nil {
		t.Fatalf("elide.Add(%+v): %v", e, err)
	}
}

func TestSummarizeViewAppliesElisionsAfterBoundary(t *testing.T) {
	msgs := texts("u0", "a1", "u2", "a3", "u4", "a5", "u6")
	s := state.New()
	s.SetMessages(msgs)
	BoundaryKey.Set(s, Boundary{Index: 2, Summary: "S"})
	mustElide(t, s, elide.Elision{ID: "c1", From: 3, To: 5, Text: "E"})

	got := wrapOnce(t, Config{}, s)

	want := []llm.Message{llm.UserText(summaryMessage(Boundary{Index: 2, Summary: "S"})), msgs[2], llm.UserText("E"), msgs[5], msgs[6]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("view = %+v\nwant %+v", got, want)
	}
	if len(s.Messages()) != len(msgs) {
		t.Fatal("the elision changed State")
	}
}

func TestSummarizeBoundaryStraddlingElision(t *testing.T) {
	msgs := []llm.Message{
		llm.UserText("u0"), assistantCalls("x"), userResults("rx", "x"),
		llm.UserText("u3"), assistantCalls("y"), userResults("ry", "y"), assistantText("a6"), llm.UserText("u7"),
	}
	s := state.New()
	s.SetMessages(msgs)
	mustElide(t, s, elide.Elision{ID: "c1", From: 1, To: 6, Text: "E"})
	// A boundary inside the elision: the elision no longer fits the view
	// and is ignored, the summary covering its first part.
	BoundaryKey.Set(s, Boundary{Index: 3, Summary: "S"})

	got := wrapOnce(t, Config{}, s)

	validateView(t, got)
	want := append([]llm.Message{llm.UserText(summaryMessage(Boundary{Summary: "S"}))}, agent.View(msgs[3:])...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("view = %+v\nwant %+v", got, want)
	}
}

func TestSummarizeSettledCountsElidedView(t *testing.T) {
	big := fmt.Sprintf(`{"content":%q}`, strings.Repeat("x", 5000))
	call := func(id string) llm.Message {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{
			{Type: llm.BlockThinking, Thinking: "plan " + id, Signature: "sig-" + id}, toolUse(id, big),
		}}
	}
	s := state.New()
	s.SetMessages([]llm.Message{
		llm.UserText("go"), call("w0"), userResults("ok", "w0"),
		// An elided tool loop before the last user turn.
		llm.UserText("more"), call("w1"), userResults("ok", "w1"), call("w2"), userResults("ok", "w2"),
		llm.UserText("next"), call("w3"), userResults("ok", "w3"), call("w4"), userResults("ok", "w4"),
	})
	mustElide(t, s, elide.Elision{ID: "c1", From: 3, To: 8, Text: "E"})

	got := wrapOnce(t, Config{}, s)

	validateView(t, got)
	if len(got) != 9 || got[3].Text() != "E" {
		t.Fatalf("view = %+v, want go, w0, ok, E, next, w3, ok, w4, ok", got)
	}
	if string(got[1].Content[1].Input) == big {
		t.Fatal("the settled tool input before the elision was not shortened")
	}
	for _, i := range []int{5, 7} {
		if b := got[i].Content; string(b[1].Input) != big || b[0].Signature == "" {
			t.Fatalf("message %d of the current tool loop was changed: %+v", i, got[i])
		}
	}
}

func TestEstimateFallsBackWhenElisionsChange(t *testing.T) {
	s := state.New()
	s.SetMessages(texts("u0 "+strings.Repeat("x", 4000), "a1", "u2"))
	m := New(Config{})
	if err := m.(agent.AfterModel).AfterModel(context.Background(), s, &llm.Response{Usage: llm.Usage{Input: 50_000}}); err != nil {
		t.Fatal(err)
	}
	if got := Estimate(s, elide.View(s, 0)); got != 50_000 {
		t.Fatalf("Estimate = %d, want the observed 50000", got)
	}
	mustElide(t, s, elide.Elision{ID: "c1", From: 0, To: 2, Text: "E"})
	view := elide.View(s, 0)
	if got, want := Estimate(s, view), charCount(view)/CharsPerToken; got != want {
		t.Fatalf("Estimate after an elision = %d, want the view's estimate %d", got, want)
	}
	// Observed with c1 applied, then c1 is removed: the view grows back.
	if err := m.(agent.AfterModel).AfterModel(context.Background(), s, &llm.Response{Usage: llm.Usage{Input: 10}}); err != nil {
		t.Fatal(err)
	}
	if got := Estimate(s, elide.View(s, 0)); got != 10 {
		t.Fatalf("Estimate = %d, want the observed 10", got)
	}
	if !elide.Remove(s, "c1") {
		t.Fatal("elide.Remove = false")
	}
	view = elide.View(s, 0)
	if got, want := Estimate(s, view), charCount(view)/CharsPerToken; got != want || got < 1000 {
		t.Fatalf("Estimate after removing the elision = %d, want the view's estimate %d", got, want)
	}
}

func TestSummarizeEstimateUsesElidedView(t *testing.T) {
	big := strings.Repeat("x", 4000)
	for _, elided := range []bool{false, true} {
		t.Run(fmt.Sprintf("elided=%v", elided), func(t *testing.T) {
			s := state.New()
			// About 2000 tokens, over the 1700-token trigger of triggerWindow.
			s.SetMessages(texts("u0 "+big, "a1 "+big))
			if elided {
				mustElide(t, s, elide.Elision{ID: "c1", From: 0, To: 2, Text: "E"})
			}
			main := llmtest.New(llmtest.Turn{Text: "done"})
			summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY"})

			runWindow(t, main, testutil.Memory(t), Config{Provider: summarizer}, agent.Input{ThreadID: "t1", State: s, Messages: texts("u2")}, triggerWindow)

			want := 1
			if elided {
				want = 0
			}
			if summarizer.Calls() != want {
				t.Fatalf("summarizer called %d times, want %d", summarizer.Calls(), want)
			}
		})
	}
}

func TestTranscriptExported(t *testing.T) {
	msgs := []llm.Message{
		llm.UserText("list files"), assistantCalls("x"), userResults("main.go", "x"), assistantText("there is main.go"),
	}
	got := Transcript(msgs)
	for _, want := range []string{"## user\nlist files", "tool_call run {\"cmd\":\"ls\"}", "tool_result x: main.go", "## assistant\nthere is main.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Transcript = %q, missing %q", got, want)
		}
	}
	if got != render(msgs, noBlockCap) {
		t.Fatal("Transcript differs from the offloaded rendering")
	}
}
