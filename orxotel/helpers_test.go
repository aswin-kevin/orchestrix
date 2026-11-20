package orxotel_test

import (
	"context"
	"iter"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/orxotel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newRecorder returns a span recorder and a tracer provider that feeds it.
func newRecorder(t *testing.T) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec, tp
}

// newAgent builds an agent named name that replays turns and is traced
// with cfg.
func newAgent(t *testing.T, name string, cfg orxotel.Config, turns []llmtest.Turn, opts ...orchestrix.Option) *agent.Agent {
	t.Helper()
	opts = append([]orchestrix.Option{
		orchestrix.WithMiddleware(orxotel.New(cfg)),
		orchestrix.WithConfig(func(c *agent.Config) { c.Name = name }),
	}, opts...)
	a, err := orchestrix.NewWithProvider(llmtest.New(turns...), opts...)
	if err != nil {
		t.Fatalf("orchestrix.New: %v", err)
	}
	return a
}

// prompt returns the input of a run asking text.
func prompt(text string) agent.Input {
	return agent.Input{Messages: []llm.Message{llm.UserText(text)}}
}

// drain reads every event of a run and returns its Final. A failed run
// yields its error alongside the final event, so errors are only fatal when
// no final event arrives.
func drain(t *testing.T, events iter.Seq2[agent.Event, error]) *agent.Final {
	t.Helper()
	var final *agent.Final
	var lastErr error
	for ev, err := range events {
		if err != nil {
			lastErr = err
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatalf("run ended without a final event (last error: %v)", lastErr)
	}
	return final
}

// spansNamed returns the ended spans called name.
func spansNamed(rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// oneSpan returns the only ended span called name, failing otherwise.
func oneSpan(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := spansNamed(rec, name)
	if len(spans) != 1 {
		var names []string
		for _, s := range rec.Ended() {
			names = append(names, s.Name())
		}
		t.Fatalf("want 1 span %q, got %d (ended: %q)", name, len(spans), names)
	}
	return spans[0]
}

// attrs returns a span's attributes by key.
func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value)
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value
	}
	return m
}

// wantAttr fails unless the span has key set to want.
func wantAttr(t *testing.T, s sdktrace.ReadOnlySpan, key string, want any) {
	t.Helper()
	v, ok := attrs(s)[attribute.Key(key)]
	if !ok {
		t.Errorf("span %q: attribute %s missing", s.Name(), key)
		return
	}
	if got := v.AsInterface(); got != want {
		t.Errorf("span %q: %s = %#v, want %#v", s.Name(), key, got, want)
	}
}

// noAttr fails if the span has key.
func noAttr(t *testing.T, s sdktrace.ReadOnlySpan, key string) {
	t.Helper()
	if v, ok := attrs(s)[attribute.Key(key)]; ok {
		t.Errorf("span %q: attribute %s = %v, want it absent", s.Name(), key, v.AsInterface())
	}
}
