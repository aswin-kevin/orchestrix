package orxotel_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/orxotel"
	"go.opentelemetry.io/otel/attribute"
)

func TestNewRejectsNegativeMaxContentBytes(t *testing.T) {
	mw, err := orxotel.New(orxotel.Config{MaxContentBytes: -1})
	if err == nil || !strings.Contains(err.Error(), "orxotel: MaxContentBytes is negative") {
		t.Fatalf("New(-1) = %v, %v; want the negative MaxContentBytes error", mw, err)
	}
	if mw != nil {
		t.Errorf("New(-1) returned a middleware alongside the error")
	}
}

func TestNameConstant(t *testing.T) {
	mw, err := orxotel.New(orxotel.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if orxotel.Name != "otel" || mw.Name() != orxotel.Name {
		t.Errorf("Name = %q, mw.Name() = %q, want both \"otel\"", orxotel.Name, mw.Name())
	}
}

func TestDefaultMaxContentBytesCapsZeroConfig(t *testing.T) {
	rec, tp := newRecorder(t)
	big := strings.Repeat("a", orxotel.DefaultMaxContentBytes+100)
	a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp, CaptureContent: true}, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{{ID: "c1", Name: "echo", Input: map[string]any{"text": big}}}},
		{Text: "done"},
	}, orchestrix.WithTools(echoTool("echo")))

	drain(t, a.Run(context.Background(), prompt("hi")))

	v := attrs(oneSpan(t, rec, "execute_tool echo"))["gen_ai.tool.call.result"]
	if got := len(v.AsString()); got != orxotel.DefaultMaxContentBytes {
		t.Errorf("captured result is %d bytes, want %d", got, orxotel.DefaultMaxContentBytes)
	}
}

func TestAttributesOnEverySpan(t *testing.T) {
	rec, tp := newRecorder(t)
	var calls atomic.Int32
	cfg := orxotel.Config{
		TracerProvider: tp,
		Attributes: func(context.Context, agent.RunInfo) []attribute.KeyValue {
			calls.Add(1)
			return []attribute.KeyValue{attribute.String("tenant", "acme")}
		},
	}
	a := newAgent(t, "worker", cfg, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{{ID: "c1", Name: "echo", Input: map[string]any{"text": "x"}}}},
		{Text: "done"},
	}, orchestrix.WithTools(echoTool("echo")))

	drain(t, a.Run(context.Background(), prompt("hi")))

	if n := calls.Load(); n != 1 {
		t.Errorf("Attributes called %d times, want 1", n)
	}
	run := oneSpan(t, rec, "invoke_agent worker")
	tool := oneSpan(t, rec, "execute_tool echo")
	chats := spansNamed(rec, "chat")
	if len(chats) != 2 {
		t.Fatalf("got %d chat spans, want 2", len(chats))
	}
	for _, s := range append(chats, run, tool) {
		wantAttr(t, s, "tenant", "acme")
	}
	// The package's own attributes are untouched.
	wantAttr(t, run, "gen_ai.operation.name", "invoke_agent")
}

func TestAttributesSeeRunInfo(t *testing.T) {
	rec, tp := newRecorder(t)
	var got agent.RunInfo
	cfg := orxotel.Config{
		TracerProvider: tp,
		Attributes: func(_ context.Context, info agent.RunInfo) []attribute.KeyValue {
			got = info
			return []attribute.KeyValue{attribute.String("thread", info.ThreadID)}
		},
	}
	a := newAgent(t, "worker", cfg, []llmtest.Turn{{Text: "done"}})

	in := agent.Input{Messages: []llm.Message{llm.UserText("hi")}, ThreadID: "t1"}
	drain(t, a.Run(context.Background(), in))

	if got.ThreadID != "t1" {
		t.Errorf("info.ThreadID = %q, want t1", got.ThreadID)
	}
	wantAttr(t, oneSpan(t, rec, "invoke_agent worker"), "thread", "t1")
}

func TestAttributesNeverReplaceOwn(t *testing.T) {
	rec, tp := newRecorder(t)
	cfg := orxotel.Config{
		TracerProvider: tp,
		Attributes: func(context.Context, agent.RunInfo) []attribute.KeyValue {
			return []attribute.KeyValue{
				attribute.String("gen_ai.operation.name", "hijack"),
				attribute.String("gen_ai.tool.name", "x"),
				attribute.String("gen_ai.request.model", "y"),
				attribute.String("tenant", "acme"),
			}
		},
	}
	a := newAgent(t, "worker", cfg, []llmtest.Turn{
		{ToolCalls: []llmtest.Call{{ID: "c1", Name: "echo", Input: map[string]any{"text": "x"}}}},
		{Text: "done"},
	}, orchestrix.WithTools(echoTool("echo")))

	drain(t, a.Run(context.Background(), prompt("hi")))

	run := oneSpan(t, rec, "invoke_agent worker")
	tool := oneSpan(t, rec, "execute_tool echo")
	chats := spansNamed(rec, "chat")
	if len(chats) != 2 {
		t.Fatalf("got %d chat spans, want 2", len(chats))
	}
	wantAttr(t, run, "gen_ai.operation.name", "invoke_agent")
	wantAttr(t, tool, "gen_ai.operation.name", "execute_tool")
	wantAttr(t, tool, "gen_ai.tool.name", "echo")
	noAttr(t, run, "gen_ai.tool.name")
	noAttr(t, run, "gen_ai.request.model")
	for _, c := range chats {
		wantAttr(t, c, "gen_ai.operation.name", "chat")
		if v := attrs(c)["gen_ai.request.model"].AsString(); v == "y" {
			t.Errorf("chat span model was replaced by the host: %q", v)
		}
		noAttr(t, c, "gen_ai.tool.name")
	}
	for _, s := range append(chats, run, tool) {
		wantAttr(t, s, "tenant", "acme")
	}
}
