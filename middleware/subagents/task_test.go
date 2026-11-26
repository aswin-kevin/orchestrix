package subagents

import (
	"context"
	"errors"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// TestTaskNilFinal settles a child run whose stream ended without a
// KindFinal: the task call gets an error result rather than a panic.
func TestTaskNilFinal(t *testing.T) {
	m := newMiddleware(t, Config{}).(*Middleware)
	empty := func(func(agent.Event, error) bool) {}
	final := drain(t.Context(), empty)
	if final != nil {
		t.Fatalf("drain of an empty stream = %+v", final)
	}
	c := &tool.Call{State: state.New()}
	for _, schema := range []map[string]any{nil, {"type": "object"}} {
		res, err := m.settle(t.Context(), c, childRun{typ: "general-purpose", schema: schema}, final)
		if err != nil || !res.IsError || len(res.Content) != 1 || res.Content[0].Text != "subagent general-purpose ended without a result" {
			t.Fatalf("settle(nil final, schema %v) = %+v, %v", schema, res, err)
		}
	}

	// A cancelled call reports the cancellation, as for any other child run.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.settle(ctx, c, childRun{typ: "general-purpose"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("settle(nil final) on a cancelled call = %v", err)
	}
}

// TestOutcomeTokenLimitIsSoftStop returns the child's text, not an error,
// when the child hit its token cap.
func TestOutcomeTokenLimitIsSoftStop(t *testing.T) {
	final := &agent.Final{Reason: agent.ReasonTokenLimit, Text: "partial"}
	res, err := outcome(t.Context(), "worker", final)
	if err != nil || res.IsError || len(res.Content) != 1 ||
		res.Content[0].Text != "partial\n[subagent stopped: token_limit]" {
		t.Fatalf("outcome = %+v, %v; want the text with a stopped note", res, err)
	}
}

// TestOutcomeRefusal reports a child's refusal with its text, not as a
// failure with a nil error.
func TestOutcomeRefusal(t *testing.T) {
	for text, want := range map[string]string{
		"I can't help with that.": "subagent worker refused: I can't help with that.",
		"":                        "subagent worker refused",
	} {
		res, err := outcome(t.Context(), "worker", &agent.Final{Reason: agent.ReasonRefusal, Text: text})
		if err != nil || !res.IsError || len(res.Content) != 1 || res.Content[0].Text != want {
			t.Errorf("outcome(refusal %q) = %+v, %v; want error result %q", text, res, err, want)
		}
	}
}
