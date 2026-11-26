package subagents

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

func TestStaleSuspendedPrunedOnNewRun(t *testing.T) {
	f := newInterruptFixture(t, nil)
	s := f.interrupt(t)
	if entries, _ := suspendedKey.Get(s); len(entries) != 1 {
		t.Fatalf("suspended entries = %v, want the interrupted call's", entries)
	}
	// A new Run abandons the interrupt, and with it the suspended child. (A
	// Resume run keeps it: TestChildInterruptSurfacesAndResumes.)
	in := userInput("never mind")
	in.State = s
	_, final := collect(t, context.Background(), f.agent, in)
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	if entries, ok := suspendedKey.Get(final.State); ok {
		t.Fatalf("suspended entries = %v, want none after the interrupt was abandoned", entries)
	}
}

type confirmInput struct{}

// confirmTool raises an interrupt allowing only allowed, and reports the
// decision it is re-invoked with.
func confirmTool(allowed ...agent.DecisionType) tool.Tool {
	return tool.New("confirm", "Confirms.", func(_ context.Context, _ confirmInput, c *tool.Call) (tool.Result, error) {
		if c.Resume == nil {
			return tool.Result{}, &agent.Interrupt{Requests: []agent.ActionRequest{{Reason: "confirm?", Allowed: allowed}}}
		}
		return tool.Text("confirmed: " + string(c.Resume.Type)), nil
	}, tool.WithReadOnly())
}

// confirmParent is a parent whose confirmer subagent calls confirm.
func confirmParent(t *testing.T, allowed ...agent.DecisionType) (*agent.Agent, *llmtest.Scripted) {
	t.Helper()
	child := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: "confirm"}}},
		llmtest.Turn{Text: "child finished"},
	)
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "confirmer", "confirm it")}},
		llmtest.Turn{Text: "parent done"},
	)
	mw := newMiddleware(t, Config{Subagents: []Subagent{{
		Name: "confirmer", Description: "Confirms.", SystemPrompt: "you confirm", Provider: child,
		Tools: []tool.Tool{confirmTool(allowed...)},
	}}})
	a, err := agent.New(agent.Config{Provider: parent, Backend: testutil.Memory(t), Middleware: []agent.Middleware{mw}})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a, parent
}

func TestChildNeedsUnrelayableDecision(t *testing.T) {
	a, parent := confirmParent(t, agent.Respond)
	_, final := collect(t, context.Background(), a, userInput("go"))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed: the parent cannot relay a respond", final.Reason, final.Err)
	}
	res := onlyResult(t, parent)
	if !res.IsError || !strings.Contains(blockText(res), "cannot relay") || !strings.Contains(blockText(res), "confirm(") {
		t.Fatalf("task result = %q (error=%v), want an error naming the unrelayable request", blockText(res), res.IsError)
	}
	if entries, ok := suspendedKey.Get(final.State); ok && len(entries) > 0 {
		t.Fatalf("suspended entries = %v, want none", entries)
	}
}

func TestParentAllowsOnlyWhatChildAllows(t *testing.T) {
	a, _ := confirmParent(t, agent.Approve, agent.Respond)
	_, final := collect(t, context.Background(), a, userInput("go"))
	pending, ok := agent.Pending(final.State)
	if final.Reason != agent.ReasonInterrupted || !ok {
		t.Fatalf("reason = %s, want interrupted", final.Reason)
	}
	if got := pending.Requests[0].Allowed; !slices.Equal(got, []agent.DecisionType{agent.Approve}) {
		t.Fatalf("allowed = %v, want only approve", got)
	}
}

func TestInvalidDecisionKeepsSuspendedChild(t *testing.T) {
	f := newInterruptFixture(t, nil)
	s := f.interrupt(t)
	// Make the stored child request refuse approval, as if the parent's
	// request had been widened: the decision must be refused before the
	// entry is taken.
	entries, _ := suspendedKey.Get(s)
	entry := entries["t1"]
	entry.Requests = slices.Clone(entry.Requests)
	entry.Requests[0].Allowed = []agent.DecisionType{agent.Reject}
	suspendedKey.Set(s, map[string]Suspended{"t1": entry})

	final := f.resume(t, s, agent.Decision{ToolUseID: "t1", Type: agent.Approve})

	if res := onlyResult(t, f.parent); !res.IsError || !strings.Contains(blockText(res), "approve") {
		t.Fatalf("task result = %q (error=%v), want an error naming the refused decision", blockText(res), res.IsError)
	}
	if kept, _ := suspendedKey.Get(final.State); len(kept) != 1 {
		t.Fatalf("suspended entries = %v, want the child kept", kept)
	}
	if f.child.Calls() != 1 || f.writes.Load() != 0 {
		t.Fatal("the child was resumed despite the refused decision")
	}
}

// TestParallelSuspendedTasksResume runs two task calls that both need
// approval. Usually both start before either suspends, and one interrupt
// covers both. But the scheduler starts no further call once one is pending,
// so if task a suspends before task b starts, b runs only on resume and asks
// in a second round; with one call at a time that always happens. Either
// way, approving every pending task until the run completes runs both, with
// their answers in call order.
func TestParallelSuspendedTasksResume(t *testing.T) {
	t.Run("parallel", func(t *testing.T) { testSuspendedTasksResume(t, 0, 0) })
	t.Run("one at a time", func(t *testing.T) { testSuspendedTasksResume(t, 1, 2) })
}

// testSuspendedTasksResume runs the scenario with the given MaxConcurrency (0
// for the default); wantRounds, when not 0, is the number of interrupts the
// run must take.
func testSuspendedTasksResume(t *testing.T, maxConcurrency, wantRounds int) {
	var writes atomic.Int32
	childTurns := func(text string) *llmtest.Scripted {
		return llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w1", Name: "write_file", Input: obj{"text": text}}}},
			llmtest.Turn{Text: "finished " + text},
		)
	}
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("ta", "a", "write a"), taskCall("tb", "b", "write b")}},
		llmtest.Turn{Text: "parent done"},
	)
	writer := []tool.Tool{approvalWriter(&writes)}
	mw := newMiddleware(t, Config{Subagents: []Subagent{
		{Name: "a", Description: "A.", SystemPrompt: "a", Provider: childTurns("a"), Tools: writer},
		{Name: "b", Description: "B.", SystemPrompt: "b", Provider: childTurns("b"), Tools: writer},
	}})
	a, err := agent.New(agent.Config{
		Provider: parent, Backend: testutil.Memory(t), Middleware: []agent.Middleware{mw},
		MaxConcurrency: maxConcurrency,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, final := collect(t, context.Background(), a, userInput("go"))
	rounds := 0
	for final.Reason == agent.ReasonInterrupted {
		rounds++
		if rounds > 2 {
			t.Fatal("still interrupted after two rounds of approvals")
		}
		pending, _ := agent.Pending(final.State)
		entries, _ := suspendedKey.Get(final.State)
		if len(entries) != len(pending.Requests) {
			t.Fatalf("round %d: %d suspended entries for %d pending calls", rounds, len(entries), len(pending.Requests))
		}
		var decisions []agent.Decision
		for _, req := range pending.Requests {
			decisions = append(decisions, agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Approve})
		}
		state := final.State
		final = nil
		for ev := range a.Resume(context.Background(), "thread", state, decisions) {
			if ev.Kind == agent.KindFinal {
				final = ev.Final
			}
		}
		if final == nil {
			t.Fatalf("round %d: resume produced no final event", rounds)
		}
	}
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("final reason = %s (%v), want completed", final.Reason, final.Err)
	}
	if wantRounds != 0 && rounds != wantRounds {
		t.Fatalf("took %d rounds of approvals, want %d", rounds, wantRounds)
	}
	if writes.Load() != 2 {
		t.Fatalf("write_file ran %d times, want twice", writes.Load())
	}
	results := toolResults(parent.Requests[1])
	if len(results) != 2 || blockText(results[0]) != "finished a" || blockText(results[1]) != "finished b" {
		t.Fatalf("task results = %+v, want both children's answers in call order", results)
	}
	if entries, _ := suspendedKey.Get(final.State); len(entries) != 0 {
		t.Fatalf("suspended entries = %v, want none", entries)
	}
}

func TestForkSuspendsAndResumes(t *testing.T) {
	var writes atomic.Int32
	// Parent and fork share the provider; the order is deterministic.
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", Fork, "write it")}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w1", Name: "write_file", Input: obj{"text": "forked"}}}},
		// After the resume: the fork tries to delegate, then answers.
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("inner", GeneralPurpose, "delegate")}},
		llmtest.Turn{Text: "fork done"},
		llmtest.Turn{Text: "parent done"},
	)
	be := testutil.Memory(t)
	a, err := agent.New(agent.Config{
		Provider: p, Backend: be, Tools: []tool.Tool{approvalWriter(&writes)},
		Middleware: []agent.Middleware{newMiddleware(t, Config{})},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, final := collect(t, context.Background(), a, userInput("go"))
	pending, ok := agent.Pending(final.State)
	if final.Reason != agent.ReasonInterrupted || !ok || !strings.HasPrefix(pending.Requests[0].Reason, "Subagent fork needs approval: write_file(") {
		t.Fatalf("final = %s, pending = %+v; want the fork's write surfaced", final.Reason, pending)
	}

	var resumed *agent.Final
	for ev := range a.Resume(context.Background(), "thread", final.State, []agent.Decision{{ToolUseID: "t1", Type: agent.Approve}}) {
		if ev.Kind == agent.KindFinal {
			resumed = ev.Final
		}
	}
	if resumed.Reason != agent.ReasonCompleted || resumed.Text != "parent done" {
		t.Fatalf("resumed = %s %q (%v), want completed", resumed.Reason, resumed.Text, resumed.Err)
	}
	if writes.Load() != 1 {
		t.Fatalf("write_file ran %d times, want once", writes.Load())
	}
	if got, err := be.Read(context.Background(), "/approved.md", 0, 0); err != nil || !slices.Equal(got.Lines, []string{"forked"}) {
		t.Fatalf("parent backend = %+v, %v; want the fork's file", got, err)
	}
	inner := toolResults(p.Requests[3])
	if len(inner) != 1 || !inner[0].IsError || blockText(inner[0]) != forkCannotDelegate {
		t.Fatalf("resumed fork's task result = %+v, want the cannot-delegate error", inner)
	}
	last := p.Requests[4].Messages
	if res := last[len(last)-1]; res.Role != llm.RoleUser || toolResults(p.Requests[4])[0].ToolUseID != "t1" ||
		blockText(toolResults(p.Requests[4])[0]) != "fork done" {
		t.Fatalf("parent's task result = %+v, want the fork's answer", toolResults(p.Requests[4]))
	}
}

func TestSuspendedForShowsFullChildRequests(t *testing.T) {
	f := newInterruptFixture(t, nil)
	s := f.interrupt(t)

	got, ok := SuspendedFor(s, "t1")
	if !ok {
		t.Fatal("SuspendedFor found no child under the interrupted task call")
	}
	if got.Type != "writer" || len(got.Requests) != 1 {
		t.Fatalf("suspended = %+v, want the writer with one request", got)
	}
	req := got.Requests[0]
	if req.Tool != "write_file" || string(req.Input) != `{"text":"hello"}` || req.Reason == "" {
		t.Fatalf("child request = %+v, want write_file with its full input and reason", req)
	}
	// A deep copy: changing it leaves the parent's State alone.
	got.Requests[0].Input[2] = 'X'
	got.Requests[0].Allowed[0] = "changed"
	again, _ := SuspendedFor(s, "t1")
	if string(again.Requests[0].Input) != `{"text":"hello"}` || again.Requests[0].Allowed[0] == "changed" {
		t.Fatalf("SuspendedFor shares memory with the State: %+v", again.Requests[0])
	}
	if _, ok := SuspendedFor(s, "t2"); ok {
		t.Fatal("SuspendedFor found a child under an unknown call")
	}
}

func TestSuspendedForKeepsInputTheReasonTruncates(t *testing.T) {
	long := json.RawMessage(`{"text":"` + strings.Repeat("x", 2*maxReasonInput) + `"}`)
	requests := []agent.ActionRequest{{ToolUseID: "w1", Tool: "write_file", Input: long, Reason: "needs approval"}}
	s := state.New()
	suspendedKey.Set(s, map[string]Suspended{"t1": {Type: "writer", Requests: requests}})

	if reason := approvalReason("writer", requests); strings.Contains(reason, string(long)) {
		t.Fatalf("parent reason quotes the whole input: %q", reason)
	}
	got, _ := SuspendedFor(s, "t1")
	if string(got.Requests[0].Input) != string(long) {
		t.Fatalf("suspended input = %s, want it whole", got.Requests[0].Input)
	}
}
