package agenttest_test

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/agenttest"
)

type note struct{ text string }

func TestAgenttestContext(t *testing.T) {
	ctx, rec := agenttest.Context(t.Context(), agent.RunInfo{ThreadID: "t", RunID: "r"})

	info, ok := agent.Info(ctx)
	if !ok || info.ThreadID != "t" || info.RunID != "r" {
		t.Fatalf("Info = %+v, %v; want thread t and run r", info, ok)
	}
	if len(info.Tools()) != 0 {
		t.Fatalf("Tools = %v, want none without an Agent", info.Tools())
	}
	if id, ok := agent.ThreadID(ctx); id != "t" || !ok {
		t.Fatalf("ThreadID = (%q, %v), want (t, true)", id, ok)
	}
	if !agent.Emit(ctx, agent.Event{Kind: agent.KindCustom, Data: note{"hi"}}) {
		t.Fatal("Emit returned false")
	}
	if agent.Emit(ctx, agent.Event{Kind: agent.KindFinal, Final: &agent.Final{}}) {
		t.Fatal("Emit of KindFinal reported success")
	}
	events := rec.Events()
	if len(events) != 1 || events[0].Kind != agent.KindCustom || events[0].Data != (note{"hi"}) {
		t.Fatalf("Events = %+v, want the one custom note", events)
	}
	events[0].Kind = agent.KindText
	if rec.Events()[0].Kind != agent.KindCustom {
		t.Fatal("Events returned the recorder's own slice, not a copy")
	}
}
