package orxotel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/orxotel"
	"github.com/aswin-kevin/orchestrix/tool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type textInput struct {
	Text string `json:"text"`
}

// echoTool returns its input text; it is read-only and concurrency-safe.
func echoTool(name string) tool.Tool {
	return tool.New(name, "Echo the text back.",
		func(_ context.Context, in textInput, _ *tool.Call) (tool.Result, error) {
			return tool.Text(in.Text), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())
}

// failTool returns an error result with the text "boom".
func failTool() tool.Tool {
	return tool.New("fail", "Always fails.",
		func(_ context.Context, _ textInput, _ *tool.Call) (tool.Result, error) {
			return tool.Errorf("boom"), nil
		},
		tool.WithReadOnly())
}

// withModel sets the agent's model.
func withModel(model string) orchestrix.Option {
	return orchestrix.WithConfig(func(c *agent.Config) { c.Model = model })
}

// wantChild fails unless child's parent is parent, in the same trace.
func wantChild(t *testing.T, child, parent sdktrace.ReadOnlySpan) {
	t.Helper()
	if child.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("span %q: parent = %v, want %q (%v)", child.Name(),
			child.Parent().SpanID(), parent.Name(), parent.SpanContext().SpanID())
	}
	if child.SpanContext().TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("span %q: trace %v, want %v", child.Name(),
			child.SpanContext().TraceID(), parent.SpanContext().TraceID())
	}
}

func TestChatSpan(t *testing.T) {
	for _, provider := range []string{"", "anthropic"} {
		t.Run("provider="+provider, func(t *testing.T) {
			rec, tp := newRecorder(t)
			a := newAgent(t, "researcher", orxotel.Config{TracerProvider: tp, ProviderName: provider}, []llmtest.Turn{
				{Text: "hello", StopReason: "end_turn", Usage: llm.Usage{Input: 12, Output: 5, CacheRead: 3}},
			}, withModel("test-model"))

			drain(t, a.Run(context.Background(), prompt("hi")))

			run := oneSpan(t, rec, "invoke_agent researcher")
			s := oneSpan(t, rec, "chat test-model")
			wantChild(t, s, run)
			if s.SpanKind() != trace.SpanKindClient {
				t.Errorf("kind = %v, want client", s.SpanKind())
			}
			wantAttr(t, s, "gen_ai.operation.name", "chat")
			wantAttr(t, s, "gen_ai.request.model", "test-model")
			if _, ok := attrs(s)["gen_ai.request.max_tokens"]; !ok {
				t.Error("gen_ai.request.max_tokens missing")
			}
			got := attrs(s)["gen_ai.response.finish_reasons"].AsStringSlice()
			if len(got) != 1 || got[0] != "end_turn" {
				t.Errorf("finish_reasons = %q, want [end_turn]", got)
			}
			wantAttr(t, s, "gen_ai.usage.input_tokens", int64(12))
			wantAttr(t, s, "gen_ai.usage.output_tokens", int64(5))
			wantAttr(t, s, "orchestrix.usage.cache_read_tokens", int64(3))
			wantAttr(t, s, "orchestrix.turn", int64(0))
			noAttr(t, s, "gen_ai.request.temperature")
			if provider == "" {
				noAttr(t, s, "gen_ai.provider.name")
			} else {
				wantAttr(t, s, "gen_ai.provider.name", provider)
			}
			if s.Status().Code != codes.Unset {
				t.Errorf("status = %v, want unset", s.Status().Code)
			}
		})
	}
}

func TestChatSpanError(t *testing.T) {
	rec, tp := newRecorder(t)
	a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
		{Err: errors.New("model exploded")},
	}, withModel("m"))

	drain(t, a.Run(context.Background(), prompt("hi")))

	s := oneSpan(t, rec, "chat m")
	if s.Status().Code != codes.Error {
		t.Errorf("status = %v, want error", s.Status().Code)
	}
	var found bool
	for _, ev := range s.Events() {
		if ev.Name == "exception" {
			found = true
		}
	}
	if !found {
		t.Errorf("no exception event recorded (events: %v)", s.Events())
	}
}

func TestToolSpan(t *testing.T) {
	rec, tp := newRecorder(t)
	a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{{ID: "call-1", Name: "echo", Input: map[string]any{"text": "hi"}}}},
		{Text: "done"},
	}, orchestrix.WithTools(echoTool("echo")))

	drain(t, a.Run(context.Background(), prompt("hi")))

	run := oneSpan(t, rec, "invoke_agent worker")
	s := oneSpan(t, rec, "execute_tool echo")
	wantChild(t, s, run)
	if s.SpanKind() != trace.SpanKindInternal {
		t.Errorf("kind = %v, want internal", s.SpanKind())
	}
	wantAttr(t, s, "gen_ai.operation.name", "execute_tool")
	wantAttr(t, s, "gen_ai.tool.name", "echo")
	wantAttr(t, s, "gen_ai.tool.call.id", "call-1")
	wantAttr(t, s, "orchestrix.tool.approved", false)
	noAttr(t, s, "orchestrix.tool.via")
	noAttr(t, s, "orchestrix.tool.is_error")
	if s.Status().Code != codes.Unset {
		t.Errorf("status = %v, want unset", s.Status().Code)
	}
}

func TestToolErrorResult(t *testing.T) {
	rec, tp := newRecorder(t)
	a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{{ID: "c1", Name: "fail", Input: map[string]any{"text": "x"}}}},
		{Text: "done"},
	}, orchestrix.WithTools(failTool()))

	drain(t, a.Run(context.Background(), prompt("hi")))

	s := oneSpan(t, rec, "execute_tool fail")
	if s.Status().Code != codes.Error {
		t.Errorf("status = %v, want error", s.Status().Code)
	}
	if d := s.Status().Description; d != "" {
		t.Errorf("status description = %q, want empty", d)
	}
	wantAttr(t, s, "orchestrix.tool.is_error", true)
}

func TestNoContentByDefault(t *testing.T) {
	rec, tp := newRecorder(t)
	a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{
			{ID: "c1", Name: "echo", Input: map[string]any{"text": "secret"}},
			{ID: "c2", Name: "fail", Input: map[string]any{"text": "secret"}},
		}},
		{Text: "done"},
	}, orchestrix.WithTools(echoTool("echo"), failTool()))

	drain(t, a.Run(context.Background(), prompt("hi")))

	for _, name := range []string{"execute_tool echo", "execute_tool fail"} {
		s := oneSpan(t, rec, name)
		noAttr(t, s, "gen_ai.tool.call.arguments")
		noAttr(t, s, "gen_ai.tool.call.result")
		if strings.Contains(s.Status().Description, "boom") {
			t.Errorf("span %q: status description %q leaks the tool error", name, s.Status().Description)
		}
	}
}

func TestCaptureContent(t *testing.T) {
	rec, tp := newRecorder(t)
	// "ééééé" is 10 bytes; a cut at 8 bytes keeps 4 runes.
	a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp, CaptureContent: true, MaxContentBytes: 8}, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{
			{ID: "c1", Name: "echo", Input: map[string]any{"text": "ééééé"}},
			{ID: "c2", Name: "fail", Input: map[string]any{"text": "x"}},
		}},
		{Text: "done"},
	}, orchestrix.WithTools(echoTool("echo"), failTool()))

	drain(t, a.Run(context.Background(), prompt("hi")))

	s := oneSpan(t, rec, "execute_tool echo")
	for _, key := range []string{"gen_ai.tool.call.arguments", "gen_ai.tool.call.result"} {
		v, ok := attrs(s)[attribute.Key(key)]
		if !ok {
			t.Errorf("%s missing", key)
			continue
		}
		got := v.AsString()
		if len(got) == 0 || len(got) > 8 || !utf8.ValidString(got) {
			t.Errorf("%s = %q (%d bytes), want 1..8 bytes of valid UTF-8", key, got, len(got))
		}
	}
	wantAttr(t, s, "gen_ai.tool.call.arguments", `{"text":`)
	wantAttr(t, s, "gen_ai.tool.call.result", "éééé")

	f := oneSpan(t, rec, "execute_tool fail")
	wantAttr(t, f, "gen_ai.tool.call.result", "boom")
	if d := f.Status().Description; d != "boom" {
		t.Errorf("status description = %q, want the captured result", d)
	}
}

func TestParallelToolSpans(t *testing.T) {
	rec, tp := newRecorder(t)
	a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{
			{ID: "a", Name: "echo", Input: map[string]any{"text": "A"}},
			{ID: "b", Name: "echo", Input: map[string]any{"text": "B"}},
			{ID: "c", Name: "lookup", Input: map[string]any{"text": "C"}},
		}},
		{Text: "done"},
	}, orchestrix.WithTools(echoTool("echo"), echoTool("lookup")))

	drain(t, a.Run(context.Background(), prompt("hi")))

	run := oneSpan(t, rec, "invoke_agent worker")
	spans := append(spansNamed(rec, "execute_tool echo"), spansNamed(rec, "execute_tool lookup")...)
	if len(spans) != 3 {
		t.Fatalf("got %d execute_tool spans, want 3", len(spans))
	}
	ids := map[string]bool{}
	for _, s := range spans {
		wantChild(t, s, run)
		ids[attrs(s)["gen_ai.tool.call.id"].AsString()] = true
	}
	if !ids["a"] || !ids["b"] || !ids["c"] {
		t.Errorf("call ids = %v, want a, b and c", ids)
	}
}

func TestSubagentSpanNesting(t *testing.T) {
	rec, tp := newRecorder(t)
	child := llmtest.New(llmtest.Turn{Text: "child done"})
	a := newAgent(t, "parent", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{{ID: "t1", Name: subagents.ToolTask,
			Input: map[string]any{"description": "review it", "subagent_type": "reviewer"}}}},
		{Text: "done"},
	}, orchestrix.WithSubagents(subagents.Subagent{
		Name: "reviewer", Description: "reviews code", SystemPrompt: "Review.", Provider: child,
	}))

	drain(t, a.Run(context.Background(), prompt("hi")))
	if child.Calls() != 1 {
		t.Fatalf("child model calls = %d, want 1", child.Calls())
	}

	parentRun := oneSpan(t, rec, "invoke_agent parent")
	task := oneSpan(t, rec, "execute_tool "+subagents.ToolTask)
	wantChild(t, task, parentRun)

	var childRun sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if strings.HasPrefix(s.Name(), "invoke_agent ") && s != parentRun {
			if childRun != nil {
				t.Fatalf("more than one child run span: %q and %q", childRun.Name(), s.Name())
			}
			childRun = s
		}
	}
	if childRun == nil {
		t.Fatal("no child run span")
	}
	wantChild(t, childRun, task)
	wantAttr(t, childRun, "orchestrix.run.depth", int64(1))

	// The child's model call nests under the child's run.
	var childChat int
	for _, s := range rec.Ended() {
		if strings.HasPrefix(s.Name(), "chat") && s.Parent().SpanID() == childRun.SpanContext().SpanID() {
			childChat++
		}
	}
	if childChat != 1 {
		t.Errorf("child chat spans = %d, want 1", childChat)
	}
}
