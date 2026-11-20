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
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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
	a, err := orchestrix.NewWithProvider(provider,
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
	if got := attrs["error"].AsString(); got != "*llm.APIError (kind=overloaded)" {
		t.Errorf("error = %q, want the error type and kind", got)
	}
}

func TestRecordFallbackNoSpan(t *testing.T) {
	orxotel.RecordFallback(context.Background(), llm.FallbackEvent{From: "a", To: "b"})
	orxotel.RecordFallback(context.Background(), llm.FallbackEvent{From: "a", To: "b", Err: context.Canceled})
}

// fallbackEvents returns the orchestrix.fallback events of a span.
func fallbackEvents(s sdktrace.ReadOnlySpan) int {
	n := 0
	for _, ev := range s.Events() {
		if ev.Name == "orchestrix.fallback" {
			n++
		}
	}
	return n
}

// TestResponseSourceDuringCooldown runs twice through a fallback provider
// whose primary fails once: the first chat span records the switch, the
// second is answered by the backup while the primary cools down, so it has
// no event; both name the backup in orchestrix.response.source.
func TestResponseSourceDuringCooldown(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		name := "streaming"
		if nonStreaming {
			name = "non-streaming"
		}
		t.Run(name, func(t *testing.T) {
			rec, tp := newRecorder(t)
			provider, err := llm.NewFallback(llm.FallbackConfig{
				Entries: []llm.FallbackEntry{
					{Provider: llmtest.New(llmtest.Turn{Err: &llm.APIError{Status: 529}}), Name: "primary"},
					{Provider: llmtest.New(llmtest.Turn{Text: "one"}, llmtest.Turn{Text: "two"}), Name: "backup"},
				},
				OnFallback: orxotel.RecordFallback,
			})
			if err != nil {
				t.Fatalf("NewFallback: %v", err)
			}
			a, err := orchestrix.NewWithProvider(provider,
				orchestrix.WithMiddleware(orxotel.New(orxotel.Config{TracerProvider: tp})),
				orchestrix.WithConfig(func(c *agent.Config) {
					c.Name = "worker"
					c.NonStreaming = nonStreaming
				}),
			)
			if err != nil {
				t.Fatalf("orchestrix.New: %v", err)
			}

			drain(t, a.Run(context.Background(), prompt("first")))
			drain(t, a.Run(context.Background(), prompt("second")))

			var chats []sdktrace.ReadOnlySpan
			for _, s := range rec.Ended() {
				if strings.HasPrefix(s.Name(), "chat") {
					chats = append(chats, s)
				}
			}
			if len(chats) != 2 {
				t.Fatalf("got %d chat spans, want 2", len(chats))
			}
			if n := fallbackEvents(chats[0]); n != 1 {
				t.Errorf("first chat span: %d fallback events, want 1", n)
			}
			wantAttr(t, chats[0], "orchestrix.response.source", "backup")
			if n := fallbackEvents(chats[1]); n != 0 {
				t.Errorf("second chat span (cool-down skip): %d fallback events, want 0", n)
			}
			wantAttr(t, chats[1], "orchestrix.response.source", "backup")
		})
	}
}

// TestNoResponseSourceWithoutFallback checks that a plain provider leaves
// orchestrix.response.source out.
func TestNoResponseSourceWithoutFallback(t *testing.T) {
	rec, tp := newRecorder(t)
	a := newAgent(t, "worker", orxotel.Config{TracerProvider: tp}, []llmtest.Turn{{Text: "ok"}})
	drain(t, a.Run(context.Background(), prompt("hi")))
	for _, s := range rec.Ended() {
		if strings.HasPrefix(s.Name(), "chat") {
			noAttr(t, s, "orchestrix.response.source")
		}
	}
}
