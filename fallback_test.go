package orchestrix_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func overloaded() llmtest.Turn {
	return llmtest.Turn{Err: &llm.APIError{Status: 529, Body: "overloaded"}}
}

func runText(t *testing.T, model llm.Provider, opts ...orchestrix.Option) string {
	t.Helper()
	a, err := orchestrix.NewWithProvider(model, opts...)
	if err != nil {
		t.Fatal(err)
	}
	var final *agent.Final
	for ev, err := range a.Run(context.Background(), orchestrix.Text("hello")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Fatalf("run did not complete: %+v", final)
	}
	return final.Text
}

func TestWithFallbackSpecs(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-fake-a")
	t.Setenv("OPENAI_API_KEY", "sk-fake-o")
	a, err := orchestrix.New("anthropic:x", orchestrix.WithFallback("openai:y"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := a.Config()
	primary, err := llm.ParseModel("anthropic:x")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := llm.New(primary)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.TypeOf(cfg.Provider) == reflect.TypeOf(plain) {
		t.Errorf("Provider is %T, want a fallback wrapper", cfg.Provider)
	}
	if cfg.Model != "x" {
		t.Errorf("Model = %q, want x", cfg.Model)
	}
	// Two backups of the same model get distinct default names.
	if _, err := orchestrix.New("anthropic:x",
		orchestrix.WithFallback("openai:gpt-4o", "openai-responses:gpt-4o")); err != nil {
		t.Errorf("two backups of one model: %v", err)
	}
}

func TestWithFallbackProviders(t *testing.T) {
	backup := llmtest.New(llmtest.Turn{Text: "hi"})
	got := runText(t, llmtest.New(overloaded()), orchestrix.WithFallback(backup),
		orchestrix.WithoutMiddleware("filesystem", "planning", "subagents"))
	if got != "hi" {
		t.Errorf("answer = %q, want hi", got)
	}
}

func TestWithFallbackRejects(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-fake-a")
	ok := llmtest.New()
	var nilProvider llm.Provider
	cfg := llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: llmtest.New()}}}
	for name, opts := range map[string][]orchestrix.Option{
		"invalid spec":  {orchestrix.WithFallback("nocolon")},
		"nil provider":  {orchestrix.WithFallback(nilProvider)},
		"nil":           {orchestrix.WithFallback(nil)},
		"wrong type":    {orchestrix.WithFallback(42)},
		"no backups":    {orchestrix.WithFallback()},
		"both options":  {orchestrix.WithFallback(llmtest.New()), orchestrix.WithFallbackConfig(cfg)},
		"both reversed": {orchestrix.WithFallbackConfig(cfg), orchestrix.WithFallback(llmtest.New())},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := orchestrix.NewWithProvider(ok, opts...); err == nil {
				t.Error("New succeeded, want an error")
			}
		})
	}
}

func TestWithFallbackConfigPrependsPrimary(t *testing.T) {
	var events []llm.FallbackEvent
	cfg := llm.FallbackConfig{
		Entries:    []llm.FallbackEntry{{Provider: llmtest.New(llmtest.Turn{Text: "from b"}), Name: "b"}},
		Cooldown:   -1,
		OnFallback: func(_ context.Context, ev llm.FallbackEvent) { events = append(events, ev) },
	}
	got := runText(t, llmtest.New(overloaded()), orchestrix.WithFallbackConfig(cfg),
		orchestrix.WithoutMiddleware("filesystem", "planning", "subagents"))
	if got != "from b" {
		t.Errorf("answer = %q, want from b", got)
	}
	if len(events) != 1 || events[0].From != "primary" || events[0].To != "b" {
		t.Errorf("events = %+v, want one primary -> b", events)
	}

	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithFallbackConfig(llm.FallbackConfig{}))
	if err == nil || !strings.Contains(err.Error(), "at least one backup") {
		t.Errorf("empty Entries: err = %v, want one mentioning at least one backup", err)
	}
}
