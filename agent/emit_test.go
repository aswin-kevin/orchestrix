package agent

import (
	"context"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// note is a custom event payload of a test's own type.
type note struct{ text string }

// emitProbe emits a KindCustom note from BeforeModel and records the result
// and the context it was given.
type emitProbe struct {
	sent *bool
	ctx  *context.Context
}

func (emitProbe) Name() string { return "emit_probe" }

func (p emitProbe) BeforeModel(ctx context.Context, _ *state.State) error {
	if p.ctx != nil {
		*p.ctx = ctx
	}
	if p.sent != nil {
		*p.sent = Emit(ctx, Event{Kind: KindCustom, Data: note{"hi"}})
	}
	return nil
}

// customEvents returns the KindCustom events of events.
func customEvents(events []Event) []Event {
	var out []Event
	for _, ev := range events {
		if ev.Kind == KindCustom {
			out = append(out, ev)
		}
	}
	return out
}

func TestEmitFromBeforeModel(t *testing.T) {
	var sent bool
	a := mustNew(t, Config{
		Provider:   llmtest.New(llmtest.Turn{Text: "done"}),
		Middleware: []Middleware{emitProbe{sent: &sent}},
	})

	events, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	if !sent {
		t.Fatal("Emit from BeforeModel returned false")
	}
	custom, text := -1, -1
	for i, ev := range events {
		switch {
		case ev.Kind == KindCustom && custom < 0:
			custom = i
		case ev.Kind == KindText && text < 0:
			text = i
		}
	}
	if custom < 0 {
		t.Fatal("stream has no KindCustom event")
	}
	ev := events[custom]
	if ev.Agent != DefaultAgentName || ev.RunID == "" || ev.RunID != events[len(events)-1].RunID || ev.Turn != 1 || ev.Time.IsZero() {
		t.Fatalf("custom event = %+v, want Agent, RunID, Turn 1 and Time set", ev)
	}
	if got, ok := ev.Data.(note); !ok || got != (note{"hi"}) {
		t.Fatalf("custom Data = %#v, want note{hi}", ev.Data)
	}
	if text < 0 {
		t.Fatal("stream has no KindText event")
	}
	if text < custom {
		t.Fatalf("custom event at %d follows the turn's text at %d", custom, text)
	}
}

func TestEmitFromToolCall(t *testing.T) {
	var sent bool
	emitter := tool.New("emitter", "Emit a note.",
		func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
			sent = Emit(ctx, Event{Kind: KindCustom, Data: note{"from tool"}})
			return tool.Text("ok"), nil
		},
		tool.WithReadOnly())
	a := mustNew(t, Config{
		Provider: llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e1", Name: "emitter"}}}, llmtest.Turn{Text: "done"}),
		Tools:    []tool.Tool{emitter},
	})

	events, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	if !sent {
		t.Fatal("Emit from a tool call returned false")
	}
	custom := customEvents(events)
	if len(custom) != 1 || custom[0].Data != (note{"from tool"}) || custom[0].ParentToolID != "" || custom[0].Turn != 1 {
		t.Fatalf("custom events = %+v, want one note from the tool with no ParentToolID", custom)
	}
}

func TestEmitRefusesFinalAndForeignCtx(t *testing.T) {
	var ctx context.Context
	var finalSent bool
	probe := emitProbe{ctx: &ctx}
	refuser := refuseFinal{sent: &finalSent}
	a := mustNew(t, Config{
		Provider:   llmtest.New(llmtest.Turn{Text: "done"}),
		Middleware: []Middleware{probe, refuser},
	})

	events, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	if finalSent {
		t.Fatal("Emit of KindFinal reported success")
	}
	finals := 0
	for _, ev := range events {
		if ev.Kind == KindFinal {
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("stream has %d KindFinal events, want 1", finals)
	}
	if Emit(context.Background(), Event{Kind: KindCustom, Data: note{"x"}}) {
		t.Fatal("Emit outside a run reported success")
	}
}

// refuseFinal tries to emit a KindFinal from BeforeModel.
type refuseFinal struct{ sent *bool }

func (refuseFinal) Name() string { return "refuse_final" }

func (p refuseFinal) BeforeModel(ctx context.Context, _ *state.State) error {
	*p.sent = Emit(ctx, Event{Kind: KindFinal, Final: &Final{}})
	return nil
}

func TestEmitAfterRunEnded(t *testing.T) {
	var ctx context.Context
	a := mustNew(t, Config{
		Provider:   llmtest.New(llmtest.Turn{Text: "done"}),
		Middleware: []Middleware{emitProbe{ctx: &ctx}},
	})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	if ctx == nil {
		t.Fatal("BeforeModel never ran")
	}
	if Emit(ctx, Event{Kind: KindCustom, Data: note{"late"}}) {
		t.Fatal("Emit succeeded after the run ended")
	}
}

func TestEmitFromNestedRun(t *testing.T) {
	var out nestedOutcome
	var sent bool
	child := mustNew(t, Config{
		Name:       "child",
		Provider:   llmtest.New(llmtest.Turn{Text: "hello from child"}),
		Middleware: []Middleware{emitProbe{sent: &sent}},
	})
	parent := mustNew(t, Config{
		Provider: llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "d1", Name: "delegate"}}}, llmtest.Turn{Text: "done"}),
		Tools:    []tool.Tool{delegateTool(child, &out)},
	})

	events, final := collect(t, parent.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	if !sent {
		t.Fatal("Emit from the child's BeforeModel returned false")
	}
	custom := customEvents(events)
	if len(custom) != 1 || custom[0].Agent != "child" || custom[0].ParentToolID != "d1" || custom[0].Data != (note{"hi"}) {
		t.Fatalf("custom events on the parent stream = %+v, want one from child under d1", custom)
	}
}

// threadProbe records ThreadID in BeforeAgent.
type threadProbe struct {
	id *string
	ok *bool
}

func (threadProbe) Name() string { return "thread_probe" }

func (p threadProbe) BeforeAgent(ctx context.Context, _ *state.State) error {
	*p.id, *p.ok = ThreadID(ctx)
	return nil
}

func TestThreadID(t *testing.T) {
	for _, tc := range []struct {
		thread string
		wantOK bool
	}{{"t1", true}, {"", false}} {
		var id string
		var ok bool
		a := mustNew(t, Config{
			Provider:   llmtest.New(llmtest.Turn{Text: "done"}),
			Middleware: []Middleware{threadProbe{&id, &ok}},
		})
		_, final := collect(t, a.Run(t.Context(), Input{ThreadID: tc.thread, Messages: []llm.Message{llm.UserText("go")}}))
		if final.Reason != ReasonCompleted {
			t.Fatalf("final = %+v, want completed", final)
		}
		if id != tc.thread || ok != tc.wantOK {
			t.Fatalf("ThreadID in a run of thread %q = (%q, %v), want (%q, %v)", tc.thread, id, ok, tc.thread, tc.wantOK)
		}
	}
	if id, ok := ThreadID(context.Background()); id != "" || ok {
		t.Fatalf("ThreadID outside a run = (%q, %v), want (\"\", false)", id, ok)
	}
}
