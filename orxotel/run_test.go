package orxotel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/orxotel"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func TestRunSpan(t *testing.T) {
	rec, tp := newRecorder(t)
	a := newAgent(t, "researcher", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
		{Text: "hello", Usage: llm.Usage{Input: 12, Output: 5, CacheRead: 3}},
	})

	final := drain(t, a.Run(context.Background(), prompt("hi")))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s, want completed", final.Reason)
	}

	s := oneSpan(t, rec, "invoke_agent researcher")
	if s.SpanKind() != trace.SpanKindInternal {
		t.Errorf("kind = %v, want internal", s.SpanKind())
	}
	if s.Parent().IsValid() {
		t.Errorf("run span has a parent %v, want a root span", s.Parent())
	}
	wantAttr(t, s, "gen_ai.operation.name", "invoke_agent")
	wantAttr(t, s, "gen_ai.agent.name", "researcher")
	wantAttr(t, s, "orchestrix.run.depth", int64(0))
	wantAttr(t, s, "orchestrix.run.resumed", false)
	wantAttr(t, s, "orchestrix.run.reason", "completed")
	wantAttr(t, s, "orchestrix.run.turns", int64(1))
	wantAttr(t, s, "gen_ai.usage.input_tokens", int64(final.Usage.Input))
	wantAttr(t, s, "gen_ai.usage.output_tokens", int64(final.Usage.Output))
	noAttr(t, s, "gen_ai.conversation.id")
	if final.Usage.Input != 12 || final.Usage.Output != 5 {
		t.Errorf("final usage = %+v, want input 12, output 5", final.Usage)
	}
	if s.Status().Code != codes.Unset {
		t.Errorf("status = %v %q, want unset", s.Status().Code, s.Status().Description)
	}
}

func TestRunSpanErrorStatus(t *testing.T) {
	t.Run("model_error", func(t *testing.T) {
		rec, tp := newRecorder(t)
		a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp, CaptureContent: true}, []llmtest.Turn{
			{Err: errors.New("model exploded")},
		})

		final := drain(t, a.Run(context.Background(), prompt("hi")))
		if final.Reason != agent.ReasonModelError {
			t.Fatalf("reason = %s, want model_error", final.Reason)
		}
		if final.Err == nil {
			t.Fatal("final.Err is nil for a model error")
		}

		s := oneSpan(t, rec, "invoke_agent worker")
		wantAttr(t, s, "orchestrix.run.reason", "model_error")
		if s.Status().Code != codes.Error {
			t.Errorf("status = %v, want error", s.Status().Code)
		}
		if got, want := s.Status().Description, final.Err.Error(); got != want {
			t.Errorf("status description = %q, want %q", got, want)
		}
	})

	t.Run("max_turns", func(t *testing.T) {
		rec, tp := newRecorder(t)
		a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
			{ToolCalls: []llmtest.Call{{ID: "c1", Name: "ls", Input: map[string]string{"path": "/"}}}},
			{Text: "never reached"},
		}, orchestrix.WithMaxTurns(1))

		final := drain(t, a.Run(context.Background(), prompt("hi")))
		if final.Reason != agent.ReasonMaxTurns {
			t.Fatalf("reason = %s, want max_turns", final.Reason)
		}

		s := oneSpan(t, rec, "invoke_agent worker")
		wantAttr(t, s, "orchestrix.run.reason", "max_turns")
		if s.Status().Code != codes.Unset {
			t.Errorf("status = %v %q, want unset", s.Status().Code, s.Status().Description)
		}
	})
}

func TestRunSpanEndsWhenConsumerStops(t *testing.T) {
	rec, tp := newRecorder(t)
	a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
		{Text: "first words"},
	})

	for ev, err := range a.Run(context.Background(), prompt("hi")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev.Kind == agent.KindText {
			break
		}
	}

	// The iterator returns only after the run has wound down, so the end
	// function has already run.
	oneSpan(t, rec, "invoke_agent worker")
	if started, ended := len(rec.Started()), len(rec.Ended()); started != ended {
		t.Errorf("started %d spans, ended %d, want every span ended", started, ended)
	}
}

func TestResumedRunSpan(t *testing.T) {
	rec, tp := newRecorder(t)
	ctx := context.Background()
	const threadID = "thread-42"
	a := newAgent(t, "writer", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{{ID: "w1", Name: "write_file",
			Input: map[string]string{"path": "/a.txt", "content": "hi\n"}}}},
		{Text: "written"},
	},
		orchestrix.WithCheckpointer(memory.New()),
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
		orchestrix.WithInterruptOn("write_file"),
	)

	th, err := a.Thread(ctx, threadID)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	first := drain(t, th.Prompt(ctx, "write a.txt"))
	if first.Reason != agent.ReasonInterrupted {
		t.Fatalf("first reason = %s, want interrupted", first.Reason)
	}

	th, err = a.Thread(ctx, threadID)
	if err != nil {
		t.Fatalf("Thread (reopen): %v", err)
	}
	pending, ok := th.Pending()
	if !ok {
		t.Fatal("no pending action after the interrupt")
	}
	var decisions []agent.Decision
	for _, req := range pending.Requests {
		decisions = append(decisions, agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Approve})
	}
	second := drain(t, th.Resume(ctx, decisions))
	if second.Reason != agent.ReasonCompleted {
		t.Fatalf("resumed reason = %s, want completed", second.Reason)
	}

	spans := spansNamed(rec, "invoke_agent writer")
	if len(spans) != 2 {
		t.Fatalf("got %d run spans, want 2", len(spans))
	}
	for i, s := range spans {
		if s.Parent().IsValid() {
			t.Errorf("run span %d has a parent, want a root span", i)
		}
		wantAttr(t, s, "gen_ai.conversation.id", threadID)
	}
	if spans[0].SpanContext().TraceID() == spans[1].SpanContext().TraceID() {
		t.Error("both runs share a trace, want one trace per run")
	}
	wantAttr(t, spans[0], "orchestrix.run.resumed", false)
	wantAttr(t, spans[0], "orchestrix.run.reason", "interrupted")
	if spans[0].Status().Code != codes.Unset {
		t.Errorf("interrupted run status = %v %q, want unset", spans[0].Status().Code, spans[0].Status().Description)
	}
	wantAttr(t, spans[1], "orchestrix.run.resumed", true)
	wantAttr(t, spans[1], "orchestrix.run.reason", "completed")
}

func TestGlobalProviderDefault(t *testing.T) {
	rec, tp := newRecorder(t)
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	a := newAgent(t, "global", orxotel.Config{}, []llmtest.Turn{{Text: "ok"}})
	drain(t, a.Run(context.Background(), prompt("hi")))

	oneSpan(t, rec, "invoke_agent global")
}

// TestGlobalProviderResolvedPerRun swaps the global provider in the middle
// of a run: every span of the run still goes to the provider that was
// global when it started.
func TestGlobalProviderResolvedPerRun(t *testing.T) {
	rec1, tp1 := newRecorder(t)
	rec2, tp2 := newRecorder(t)
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp1)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	swap := tool.New("swap", "Swaps the global tracer provider.",
		func(_ context.Context, _ textInput, _ *tool.Call) (tool.Result, error) {
			otel.SetTracerProvider(tp2)
			return tool.Text("swapped"), nil
		}, tool.WithReadOnly())
	a := newAgent(t, "global", orxotel.Config{}, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{{ID: "c1", Name: "swap", Input: map[string]any{"text": "x"}}}},
		{Text: "done"},
	}, orchestrix.WithTools(swap))
	drain(t, a.Run(context.Background(), prompt("hi")))

	if got := len(rec2.Ended()); got != 0 {
		t.Errorf("provider set mid-run got %d spans, want 0", got)
	}
	oneSpan(t, rec1, "invoke_agent global")
	oneSpan(t, rec1, "execute_tool swap")
	if got := len(spansNamed(rec1, "chat")); got != 2 {
		t.Errorf("got %d chat spans from the run's provider, want 2", got)
	}

	t.Run("token_limit", func(t *testing.T) {
		rec, tp := newRecorder(t)
		a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
			{ToolCalls: []llmtest.Call{{ID: "c1", Name: "ls", Input: map[string]string{"path": "/"}}}, Usage: llm.Usage{Input: 100}},
			{Text: "never reached"},
		}, orchestrix.WithMaxTotalTokens(100))

		final := drain(t, a.Run(context.Background(), prompt("hi")))
		if final.Reason != agent.ReasonTokenLimit {
			t.Fatalf("reason = %s, want token_limit", final.Reason)
		}

		s := oneSpan(t, rec, "invoke_agent worker")
		wantAttr(t, s, "orchestrix.run.reason", "token_limit")
		if s.Status().Code != codes.Unset {
			t.Errorf("status = %v %q, want unset", s.Status().Code, s.Status().Description)
		}
	})
}
