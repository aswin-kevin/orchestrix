// Command fallback keeps an agent answering when its model is down.
// WithFallback names a backup; when the primary fails with an overload, a rate
// limit or a network error, the call moves to the backup and the callback
// reports the switch.
//
//	go run ./examples/fallback                                   # offline, scripted
//	ORX_MODEL=anthropic:claude-sonnet-4-5 ORX_FALLBACK_MODEL=openai:gpt-4o go run ./examples/fallback
//
// Offline, the scripted primary fails with a 529 (overloaded) and the
// scripted backup answers. Live, ORX_MODEL is the primary and
// ORX_FALLBACK_MODEL the backup, and both providers' API keys must be set. A
// healthy primary answers by itself, so no switch happens; to force one,
// misconfigure the primary on purpose, for example by pointing its base URL
// (ANTHROPIC_BASE_URL or OPENAI_BASE_URL) at a closed local port so that the
// call fails with a network error.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/examples/internal/exmodel"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// fallbackEnv names the backup model's spec in live mode.
const fallbackEnv = "ORX_FALLBACK_MODEL"

func main() {
	err := exmodel.Check() // live: fail early when the provider's API key is missing
	if err == nil {
		err = run(context.Background(), os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fallback:", exmodel.Safe(err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, w io.Writer) error {
	// The primary is overloaded; the backup answers.
	primary := exmodel.Model(llmtest.Turn{Err: &llm.APIError{Status: 529, Body: "overloaded"}})
	var backup llm.Provider = llmtest.New(llmtest.Turn{Text: "Paris is the capital of France."})
	if exmodel.Live() {
		var err error
		if backup, err = liveBackup(); err != nil {
			return err
		}
	}

	a, err := orchestrix.New(primary,
		orchestrix.WithFallbackConfig(llm.FallbackConfig{
			Entries: []llm.FallbackEntry{{Provider: backup, Name: "backup"}},
			OnFallback: func(_ context.Context, ev llm.FallbackEvent) {
				fmt.Fprintf(w, "switched: %s -> %s\n", exmodel.Safe(ev.From), exmodel.Safe(ev.To))
			},
		}),
		orchestrix.WithoutMiddleware("filesystem", "planning", "subagents"), // nothing to explore: answer directly
		orchestrix.WithMaxTurns(3),
	)
	if err != nil {
		return err
	}

	var final *agent.Final
	for ev, err := range a.Run(ctx, orchestrix.Text("What is the capital of France?")) {
		if err != nil {
			return err
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		return errors.New("run ended without a final event")
	}
	if final.Reason != agent.ReasonCompleted {
		return fmt.Errorf("run stopped: %s (%v)", final.Reason, final.Err)
	}
	fmt.Fprintf(w, "answer: %s\n", exmodel.Safe(final.Text))
	return nil
}

// liveBackup builds the backup provider from ORX_FALLBACK_MODEL.
func liveBackup() (llm.Provider, error) {
	spec := os.Getenv(fallbackEnv)
	if spec == "" {
		return nil, fmt.Errorf("set %s to the backup model, for example openai:gpt-4o", fallbackEnv)
	}
	cfg, err := llm.ParseModel(spec)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fallbackEnv, err)
	}
	return llm.New(cfg)
}
