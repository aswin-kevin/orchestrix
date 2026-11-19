package orxotel_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/orxotel"
	"go.opentelemetry.io/otel/attribute"
)

func TestRecordFallback(t *testing.T) {
	rec, tp := newRecorder(t)
	provider, err := llm.NewFallback(llm.FallbackConfig{
		Entries: []llm.FallbackEntry{
			{Provider: llmtest.New(llmtest.Turn{Err: &llm.APIError{Status: 529}}), Name: "primary"},
			{Provider: llmtest.New(llmtest.Turn{Text: "ok"}), Name: "backup"},
		},
		OnFallback: orxotel.RecordFallback,
	})
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}
	a, err := orchestrix.New(provider,
		orchestrix.WithMiddleware(orxotel.New(orxotel.Config{TracerProvider: tp})),
		orchestrix.WithConfig(func(c *agent.Config) { c.Name = "worker" }),
	)
	if err != nil {
		t.Fatalf("orchestrix.New: %v", err)
	}

	drain(t, a.Run(context.Background(), prompt("hi")))

	chat := oneSpan(t, rec, "chat")
	var attrs map[attribute.Key]attribute.Value
	n := 0
	for _, ev := range chat.Events() {
		if ev.Name != "orchestrix.fallback" {
			continue
		}
		n++
		attrs = make(map[attribute.Key]attribute.Value)
		for _, kv := range ev.Attributes {
			attrs[kv.Key] = kv.Value
		}
	}
	if n != 1 {
		t.Fatalf("got %d orchestrix.fallback events on the chat span, want 1", n)
	}
	if got := attrs["from"].AsString(); got != "primary" {
		t.Errorf("from = %q, want primary", got)
	}
	if got := attrs["to"].AsString(); got != "backup" {
		t.Errorf("to = %q, want backup", got)
	}
	if got := attrs["error"].AsString(); !strings.Contains(got, "529") {
		t.Errorf("error = %q, want it to mention 529", got)
	}
}

func TestRecordFallbackNoSpan(t *testing.T) {
	orxotel.RecordFallback(context.Background(), llm.FallbackEvent{From: "a", To: "b"})
	orxotel.RecordFallback(context.Background(), llm.FallbackEvent{From: "a", To: "b", Err: context.Canceled})
}
