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
	m := newMiddleware(t, Config{}).(*middleware)
	empty := func(func(agent.Event, error) bool) {}
	final := drain(context.Background(), empty)
	if final != nil {
		t.Fatalf("drain of an empty stream = %+v", final)
	}
	c := &tool.Call{State: state.New()}
	for _, schema := range []map[string]any{nil, {"type": "object"}} {
		res, err := m.settle(context.Background(), c, childRun{typ: "general-purpose", schema: schema}, final)
		if err != nil || !res.IsError || len(res.Content) != 1 || res.Content[0].Text != "subagent general-purpose ended without a result" {
			t.Fatalf("settle(nil final, schema %v) = %+v, %v", schema, res, err)
		}
	}

	// A cancelled call reports the cancellation, as for any other child run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.settle(ctx, c, childRun{typ: "general-purpose"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("settle(nil final) on a cancelled call = %v", err)
	}
}

// TestOutcomeTokenLimitIsSoftStop returns the child's text, not an error,
// when the child hit its token cap.
func TestOutcomeTokenLimitIsSoftStop(t *testing.T) {
	final := &agent.Final{Reason: agent.ReasonTokenLimit, Text: "partial"}
	res, err := outcome(context.Background(), "worker", final)
	if err != nil || res.IsError || len(res.Content) != 1 ||
		res.Content[0].Text != "partial\n[subagent stopped: token_limit]" {
		t.Fatalf("outcome = %+v, %v; want the text with a stopped note", res, err)
	}
}
