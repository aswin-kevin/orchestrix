package compress

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/state"
)

// wrap runs m's BeforeModel and WrapModel once over s, as the loop would,
// and returns the request that reached the provider.
func wrap(t *testing.T, m agent.Middleware, s *state.State) llm.Request {
	t.Helper()
	if err := m.(agent.BeforeModel).BeforeModel(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	req := &agent.ModelRequest{
		State:   s,
		Request: llm.Request{Messages: elide.View(s, 0)},
		Profile: llm.Profile{ContextWindow: testWindow},
	}
	var sent llm.Request
	_, err := m.(agent.ModelWrapper).WrapModel(context.Background(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
		sent = r.Request
		return &llm.Response{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sent
}

func call(id string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{
		{Type: llm.BlockToolUse, ID: id, Name: "noop", Input: json.RawMessage(`{}`)},
	}}
}

func result(id string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.ToolResultText(id, "ok", false)}}
}

func TestNeverProposesCurrentTurn(t *testing.T) {
	s := state.New()
	// Turn 5 (index 10) is the run's input, answered by a tool loop still
	// in progress.
	s.SetMessages(append(history(5), llm.UserText(turnText("U", 5)), call("n1"), result("n1"), call("n2"), result("n2")))
	wrap(t, mustNew(t, Config{MinRangeTokens: 100, KeepTurns: 1}), s)

	p, ok := proposalKey.Get(s)
	if !ok || p.From != 0 || p.To != 10 {
		t.Fatalf("proposal = %+v (set %v), want [0, 10): everything before the current turn", p, ok)
	}
}

func TestRangeAvoidsDanglingCalls(t *testing.T) {
	msgs := append(history(5), llm.UserText(turnText("U", 5)))
	// Turn 1's reply made a call that was never answered.
	msgs[3] = call("dangling")
	s := state.New()
	s.SetMessages(msgs)
	wrap(t, mustNew(t, testCfg), s)

	p, ok := proposalKey.Get(s)
	if !ok || (p.From <= 3 && 3 < p.To) {
		t.Fatalf("proposal = %+v (set %v), want a range around the dangling call", p, ok)
	}
	if err := elide.Add(s, elide.Elision{ID: p.ID, From: p.From, To: p.To, Text: "x"}); err != nil {
		t.Fatalf("the proposed range is refused: %v", err)
	}
}

func TestPassedElisionsDropped(t *testing.T) {
	s := state.New()
	s.SetMessages(append(history(5), llm.UserText(turnText("U", 5))))
	for _, e := range []elide.Elision{{ID: "c1", From: 0, To: 2, Text: "E1"}, {ID: "c2", From: 2, To: 6, Text: "E2"}} {
		if err := elide.Add(s, e); err != nil {
			t.Fatal(err)
		}
	}
	// The boundary passed c1 and straddles c2.
	summarize.BoundaryKey.Set(s, summarize.Boundary{Index: 4, Summary: "S"})
	wrap(t, mustNew(t, testCfg), s)

	if e := elide.All(s); len(e) > 0 {
		t.Fatalf("elisions = %+v, want the passed ones dropped", e)
	}
	if p, _ := proposalKey.Get(s); p.ID != "c3" || p.From != 4 || p.To != 6 {
		t.Fatalf("proposal = %+v, want c3 over [4, 6)", p)
	}
}

func TestIDsSkipExistingElisions(t *testing.T) {
	s := state.New()
	s.SetMessages(append(history(5), llm.UserText(turnText("U", 5))))
	if err := elide.Add(s, elide.Elision{ID: "c1", From: 0, To: 2, Text: "E1"}); err != nil {
		t.Fatal(err)
	}
	sent := wrap(t, mustNew(t, testCfg), s)

	if p, _ := proposalKey.Get(s); p.ID != "c2" || p.From != 2 || p.To != 6 {
		t.Fatalf("proposal = %+v, want c2 over [2, 6)", p)
	}
	if id := proposed(sent); id != "c2" {
		t.Fatalf("request proposes %q, want c2", id)
	}
}

func TestSecondRunProposesNextID(t *testing.T) {
	first := llmtest.New(llmtest.Turn{Text: "done"})
	s := run(t, first, testutil.Memory(t), testCfg, seeded())
	if id := proposed(first.Requests[0]); id != "c1" {
		t.Fatalf("first run proposes %q, want c1", id)
	}
	second := llmtest.New(llmtest.Turn{Text: "done"})
	run(t, second, testutil.Memory(t), testCfg, s)
	if id := proposed(second.Requests[0]); id != "c2" {
		t.Fatalf("second run proposes %q, want c2", id)
	}
}

func TestEmptyPromptKeepsRequest(t *testing.T) {
	empty := ""
	s := state.New()
	s.SetMessages(append(history(5), llm.UserText(turnText("U", 5))))
	sent := wrap(t, mustNew(t, Config{MinRangeTokens: 100, Prompt: &empty}), s)

	sys := strings.Join(sent.System, "\n")
	if strings.Contains(sys, DefaultPrompt) || proposed(sent) != "c1" {
		t.Fatalf("system = %q, want only the compression request", sys)
	}
}

// continuing keeps a run going with one BeforeStop prompt per call, up to
// agent.MaxStopContinuations, as rubric feedback or a reminder would.
type continuing struct{}

func (continuing) Name() string { return "continuing" }

func (continuing) BeforeStop(context.Context, *state.State, *llm.Response) (string, error) {
	return "try again", nil
}

func TestStopContinuationsAreNotTurns(t *testing.T) {
	// The run's input lands at index 10; three stop continuations follow
	// its answers, as the agent core appends them.
	p := llmtest.New(llmtest.Turn{Text: "a"}, llmtest.Turn{Text: "b"}, llmtest.Turn{Text: "c"}, llmtest.Turn{Text: "d"})
	s := run(t, p, testutil.Memory(t), Config{}, seeded(), continuing{})
	if n := len(s.Messages()); n != 18 {
		t.Fatalf("state has %d messages, want 18 (input, 4 answers, 3 continuations)", n)
	}

	wrap(t, mustNew(t, testCfg), s)

	pr, ok := proposalKey.Get(s)
	if !ok || pr.To > 10 {
		t.Fatalf("proposal = %+v (set %v), want one that ends before the run's input at 10", pr, ok)
	}
}

func TestUpToQuotesWhatTheModelSees(t *testing.T) {
	s := state.New()
	s.SetMessages(append(history(6), llm.UserText(turnText("U", 6))))
	// Turns 2 and 3 are already elided: the proposal [0, 4) ends where
	// that elision starts, so the model sees its text there, not U2.
	if err := elide.Add(s, elide.Elision{ID: "x1", From: 4, To: 8, Text: "EARLIER NOTE about turns two and three"}); err != nil {
		t.Fatal(err)
	}
	sent := wrap(t, mustNew(t, testCfg), s)

	if p, _ := proposalKey.Get(s); p.From != 0 || p.To != 4 {
		t.Fatalf("proposal = %+v, want [0, 4)", p)
	}
	sys := strings.Join(sent.System, "\n")
	if !strings.Contains(sys, `Up to: "EARLIER NOTE about turns two and three"`) || strings.Contains(sys, `Up to: "U2`) {
		t.Fatalf("system = %q, want the Up to excerpt quoting the elision the model sees", sys)
	}
}
