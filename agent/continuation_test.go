package agent

import (
	"context"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/tool"
)

// continuationsIn returns the user messages of msgs after the first one,
// which are the continuations the core appended in a run started by one
// input and driven without tools.
func continuationsIn(msgs []llm.Message) []llm.Message {
	var out []llm.Message
	for i, m := range msgs {
		if i > 0 && m.Role == llm.RoleUser {
			out = append(out, m)
		}
	}
	return out
}

func TestContinuationsAreMarkedInjected(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(p *llmtest.Scripted) Config
		turn []llmtest.Turn
	}{
		{
			name: "BeforeStop",
			cfg: func(p *llmtest.Scripted) Config {
				var calls int
				return Config{Provider: p, Middleware: []Middleware{stopHook{prompts: []string{"check again", ""}, calls: &calls}}}
			},
			turn: []llmtest.Turn{{Text: "a"}, {Text: "b"}},
		},
		{
			name: "max-tokens resume",
			cfg:  func(p *llmtest.Scripted) Config { return Config{Provider: p} },
			turn: []llmtest.Turn{truncatedTurn("cut"), {Text: "done"}},
		},
		{
			name: "token-budget nudge",
			cfg:  func(p *llmtest.Scripted) Config { return Config{Provider: p, TokenBudget: 10_000} },
			turn: []llmtest.Turn{textTurn("a", 100), textTurn("b", 9_000)},
		},
		{
			name: "settlement",
			cfg: func(p *llmtest.Scripted) Config {
				return Config{Provider: p, Tools: []tool.Tool{echoTool()}, MaxTurns: 1, Settlement: &Settlement{Prompt: settlementPrompt}}
			},
			turn: []llmtest.Turn{toolTurn("t1"), {Text: "summary"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := llmtest.New(tc.turn...)
			a := mustNew(t, tc.cfg(p))

			_, final := collect(t, a.Run(context.Background(), userInput("go")))

			msgs := final.State.Messages()
			if msgs[0].Meta[InjectedMetaKey] != "" {
				t.Fatalf("the run's input is marked injected: %+v", msgs[0].Meta)
			}
			var found bool
			for _, m := range continuationsIn(msgs) {
				if m.Text() == "" {
					continue // tool results
				}
				found = true
				if got := m.Meta[InjectedMetaKey]; got != ContinuationMetaValue {
					t.Fatalf("continuation %q has %s = %q, want %q", m.Text(), InjectedMetaKey, got, ContinuationMetaValue)
				}
			}
			if !found {
				t.Fatalf("no continuation in %+v", msgs)
			}
			for _, req := range p.Requests {
				for _, m := range req.Messages {
					if m.Meta != nil {
						t.Fatalf("provider request carries Meta %+v", m.Meta)
					}
				}
			}
		})
	}
}
