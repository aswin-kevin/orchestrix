package orchestrix_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
)

// TestStopConsultationOrderAndSharedSlots pins how the preset's BeforeStop
// middleware share agent.MaxStopContinuations: async's reminder is consulted
// first, then rubric, then the hooks Stop hooks, the first non-empty prompt
// wins, and once the slots are spent no Stop hook runs on the final answer.
func TestStopConsultationOrderAndSharedSlots(t *testing.T) {
	am, err := async.New(async.Config{Runner: runForever{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(am.Shutdown)
	var log []string
	grader := rubric.GraderFunc(func(_ context.Context, req rubric.GradeRequest) (rubric.Grade, error) {
		log = append(log, "rubric: "+req.Answer)
		return rubric.Grade{Pass: req.Iteration > 0, Feedback: "Name the source."}, nil
	})
	// A quality gate that always asks for another turn.
	gate := func(_ context.Context, in hooks.StopInput) (hooks.StopOutput, error) {
		log = append(log, "hooks: "+in.Final)
		return hooks.StopOutput{Continue: "Run the tests first."}, nil
	}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s", Name: async.ToolStart,
			Input: map[string]string{"description": "Research the topic.", "subagent_type": "general-purpose"}}}},
		llmtest.Turn{Text: "answer 1"},
		llmtest.Turn{Text: "answer 2"},
		llmtest.Turn{Text: "answer 3"},
		llmtest.Turn{Text: "answer 4"},
		llmtest.Turn{Text: "spare: must stay unused"},
	)
	a, err := orchestrix.New(p,
		orchestrix.WithHooks(hooks.Config{Stop: []func(context.Context, hooks.StopInput) (hooks.StopOutput, error){gate}}),
		orchestrix.WithAsync(am),
		orchestrix.WithRubric(rubric.Config{Criteria: []string{"Cites a source."}, Grader: grader}),
	)
	if err != nil {
		t.Fatal(err)
	}

	final := runThread(t, a, "t1", "Research the topic.")

	// answer 1: the async reminder wins, rubric and hooks are not asked.
	// answer 2: rubric fails it. answer 3: rubric passes, so the hooks gate
	// is asked and continues, spending the last slot. answer 4: nobody is
	// asked; it is the final answer although the gate would block it.
	want := []string{"rubric: answer 2", "rubric: answer 3", "hooks: answer 3"}
	if !slices.Equal(log, want) {
		t.Fatalf("consultations = %q, want %q", log, want)
	}
	if final.Text != "answer 4" || p.Calls() != 2+agent.MaxStopContinuations {
		t.Fatalf("final %q after %d model calls, want answer 4 after %d", final.Text, p.Calls(), 2+agent.MaxStopContinuations)
	}
	last := func(i int) string {
		msgs := p.Requests[i].Messages
		return msgs[len(msgs)-1].Text()
	}
	for i, want := range map[int]string{2: "async_1", 3: "rubric_feedback", 4: "Run the tests first."} {
		if got := last(i); !strings.Contains(got, want) {
			t.Errorf("request %d ends with %q, want %q", i, got, want)
		}
	}
}
