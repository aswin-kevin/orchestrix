package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// answerSchema is the schema of the structured-output tests.
var answerSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"n": map[string]any{"type": "integer"},
		"s": map[string]any{"type": "string"},
	},
	"required":             []any{"n"},
	"additionalProperties": false,
}

// compactSchema is answerSchema as the prompt segment carries it.
func compactSchema(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(answerSchema)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewRejectsBadOutputSchema(t *testing.T) {
	p := llmtest.New()
	tests := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{"array", map[string]any{"type": "array"}, "object"},
		{"no type", map[string]any{"properties": map[string]any{}}, "object"},
		{"unmarshalable", map[string]any{"type": "object", "x": func() {}}, "OutputSchema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Config{Provider: p, OutputSchema: tt.schema})
			if err == nil {
				t.Fatal("New succeeded")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestOutputPromptSegment(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: `{"n":1}`})
	a := mustNew(t, Config{Provider: p, System: []string{"base"}, CacheBoundary: 1, OutputSchema: answerSchema})
	collect(t, a.Run(t.Context(), userInput("go")))
	req := p.Requests[0]
	want := []string{"base", defaultOutputPrompt + compactSchema(t)}
	if !slices.Equal(req.System, want) {
		t.Errorf("System = %q, want %q", req.System, want)
	}
	if req.CacheBoundary != 2 {
		t.Errorf("CacheBoundary = %d, want 2", req.CacheBoundary)
	}
}

func TestOutputPromptKeepsShorterCacheBoundary(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: `{"n":1}`})
	a := mustNew(t, Config{Provider: p, System: []string{"a", "b"}, CacheBoundary: 1, OutputSchema: answerSchema})
	collect(t, a.Run(t.Context(), userInput("go")))
	req := p.Requests[0]
	want := []string{"a", "b", defaultOutputPrompt + compactSchema(t)}
	if !slices.Equal(req.System, want) {
		t.Errorf("System = %q, want %q", req.System, want)
	}
	if req.CacheBoundary != 1 {
		t.Errorf("CacheBoundary = %d, want 1", req.CacheBoundary)
	}
}

func TestNoOutputSchemaNoSegment(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "hi"})
	system := []string{"a", "b"}
	a := mustNew(t, Config{Provider: p, System: system, CacheBoundary: 2})
	collect(t, a.Run(t.Context(), userInput("go")))
	req := p.Requests[0]
	if !slices.Equal(req.System, system) {
		t.Errorf("System = %q, want %q", req.System, system)
	}
	if req.CacheBoundary != 2 {
		t.Errorf("CacheBoundary = %d, want 2", req.CacheBoundary)
	}
}

// runOutput runs an agent with answerSchema whose model answers turns.
func runOutput(t *testing.T, cfg Config, turns ...llmtest.Turn) (*llmtest.Scripted, *Final) {
	t.Helper()
	p := llmtest.New(turns...)
	cfg.Provider, cfg.OutputSchema = p, answerSchema
	_, final := collect(t, mustNew(t, cfg).Run(t.Context(), userInput("go")))
	return p, final
}

// wantOutput fails unless final completed with output want.
func wantOutput(t *testing.T, final *Final, want string) {
	t.Helper()
	if final.Reason != ReasonCompleted || final.Err != nil {
		t.Fatalf("final = %+v, want completed", final)
	}
	if string(final.Output) != want {
		t.Fatalf("Output = %q, want %q", final.Output, want)
	}
}

func TestOutputValidFirstTry(t *testing.T) {
	p, final := runOutput(t, Config{}, llmtest.Turn{Text: `{"n":1}`})
	wantOutput(t, final, `{"n":1}`)
	if n := p.Calls(); n != 1 {
		t.Errorf("model calls = %d, want 1", n)
	}
}

func TestOutputFencedAnswerAccepted(t *testing.T) {
	reply := "```json\n{\"n\": 2}\n```"
	_, final := runOutput(t, Config{}, llmtest.Turn{Text: reply})
	wantOutput(t, final, `{"n":2}`)
	if final.Text != reply {
		t.Errorf("Text = %q, want the raw reply %q", final.Text, reply)
	}
}

func TestOutputCorrected(t *testing.T) {
	_, final := runOutput(t, Config{}, llmtest.Turn{Text: "sure!"}, llmtest.Turn{Text: `{"n":3}`})
	wantOutput(t, final, `{"n":3}`)
	conts := continuationsIn(final.State.Messages())
	if len(conts) != 1 {
		t.Fatalf("continuations = %+v, want one", conts)
	}
	if c := conts[0]; !strings.HasPrefix(c.Text(), "Your answer is not valid:") || c.Meta[InjectedMetaKey] != ContinuationMetaValue {
		t.Errorf("continuation = %+v, want an injected correction", c)
	}
}

func TestOutputRetriesSpent(t *testing.T) {
	p, final := runOutput(t, Config{}, llmtest.Turn{Text: "a"}, llmtest.Turn{Text: "b"}, llmtest.Turn{Text: "c"})
	if final.Reason != ReasonInvalidOutput || !errors.Is(final.Err, ErrInvalidOutput) {
		t.Fatalf("final = %+v, want invalid_output wrapping ErrInvalidOutput", final)
	}
	if final.Output != nil || final.Text != "c" {
		t.Errorf("Output = %q, Text = %q; want nil and %q", final.Output, final.Text, "c")
	}
	if n := p.Calls(); n != DefaultOutputRetries+1 {
		t.Errorf("model calls = %d, want %d", n, DefaultOutputRetries+1)
	}
}

func TestOutputRevalidatedAfterStopHook(t *testing.T) {
	var calls int
	_, final := runOutput(t, Config{Middleware: []Middleware{stopHook{prompts: []string{"revise", ""}, calls: &calls}}},
		llmtest.Turn{Text: `{"n":1}`}, llmtest.Turn{Text: `{"n":5}`})
	wantOutput(t, final, `{"n":5}`)
}

func TestOutputCheckedWhenStopSlotsSpent(t *testing.T) {
	var calls int
	turns := []llmtest.Turn{{Text: `{"n":1}`}, {Text: `{"n":1}`}, {Text: `{"n":1}`}, {Text: "bad"}, {Text: `{"n":9}`}}
	p, final := runOutput(t, Config{Middleware: []Middleware{stopHook{prompts: []string{"again", "again", "again"}, calls: &calls}}},
		turns...)
	wantOutput(t, final, `{"n":9}`)
	if n := p.Calls(); n != 5 {
		t.Errorf("model calls = %d, want 5", n)
	}
	if calls != MaxStopContinuations {
		t.Errorf("hook calls = %d, want %d", calls, MaxStopContinuations)
	}
}

func TestOutputSkippedForNestedRun(t *testing.T) {
	var a *Agent
	var nested *Final
	nest := tool.New("nest", "Run the agent again.",
		func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
			for ev := range a.Run(ctx, userInput("inner")) {
				if ev.Kind == KindFinal {
					nested = ev.Final
				}
			}
			return tool.Text(nested.Text), nil
		},
		tool.WithReadOnly())
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "n1", Name: "nest"}}},
		llmtest.Turn{Text: "free text"},
		llmtest.Turn{Text: `{"n":1}`},
	)
	a = mustNew(t, Config{Provider: p, OutputSchema: answerSchema, Tools: []tool.Tool{nest}})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	if nested == nil || nested.Reason != ReasonCompleted || nested.Output != nil || nested.Text != "free text" {
		t.Fatalf("nested final = %+v, want completed free text without Output", nested)
	}
	wantOutput(t, final, `{"n":1}`)
}

func TestOutputNilWhenNotCompleted(t *testing.T) {
	_, final := runOutput(t, Config{MaxTurns: 1, Tools: []tool.Tool{echoTool()}}, toolTurn("e1"))
	if final.Reason != ReasonMaxTurns || final.Output != nil {
		t.Fatalf("final = %+v, want max_turns without Output", final)
	}
}

func TestOutputRetriesResetOnResume(t *testing.T) {
	log := newCallLog()
	p := llmtest.New(
		// One correction is spent before the run is interrupted.
		llmtest.Turn{Text: "first"},
		llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w")}},
		llmtest.Turn{Text: "x"},
		llmtest.Turn{Text: "y"},
		llmtest.Turn{Text: `{"n":1}`},
	)
	a := mustNew(t, Config{Provider: p, OutputSchema: answerSchema, Tools: []tool.Tool{writeTool(log)}})
	_, interrupted := collect(t, a.Run(t.Context(), userInput("go")))
	if interrupted.Reason != ReasonInterrupted {
		t.Fatalf("final = %+v, want interrupted", interrupted)
	}

	_, final := collect(t, a.Resume(t.Context(), "", interrupted.State, []Decision{{ToolUseID: "w", Type: Approve}}))

	wantOutput(t, final, `{"n":1}`)
}

// failingAfterAgent turns a completed run into ReasonMiddlewareError.
type failingAfterAgent struct{}

func (failingAfterAgent) Name() string { return "failing_after_agent" }

func (failingAfterAgent) AfterAgent(context.Context, *state.State) error {
	return errors.New("after agent failed")
}

func TestOutputNilWhenAfterAgentFails(t *testing.T) {
	_, final := runOutput(t, Config{Middleware: []Middleware{failingAfterAgent{}}}, llmtest.Turn{Text: `{"n":1}`})
	if final.Reason != ReasonMiddlewareError || final.Output != nil {
		t.Fatalf("final = %+v, want middleware_error without Output", final)
	}
}

func TestCustomOutputPrompts(t *testing.T) {
	p, final := runOutput(t, Config{OutputPrompt: "JSON please:\n", InvalidOutputPrompt: "fix: %v"},
		llmtest.Turn{Text: "nope"}, llmtest.Turn{Text: `{"n":1}`})
	wantOutput(t, final, `{"n":1}`)
	if sys := p.Requests[0].System; len(sys) != 1 || sys[0] != "JSON please:\n"+compactSchema(t) {
		t.Errorf("System = %q, want the custom prompt then the schema", sys)
	}
	if got := lastText(p.Requests[1]); !strings.HasPrefix(got, "fix: ") {
		t.Errorf("correction = %q, want prefix %q", got, "fix: ")
	}
}

func TestInvalidOutputPromptVerbs(t *testing.T) {
	for prompt, want := range map[string]string{
		"try again":   "try again",
		"fix (%s)":    "fix (",
		"100%% wrong": "100% wrong",
	} {
		p, final := runOutput(t, Config{InvalidOutputPrompt: prompt}, llmtest.Turn{Text: "nope"}, llmtest.Turn{Text: `{"n":1}`})
		wantOutput(t, final, `{"n":1}`)
		got := lastText(p.Requests[1])
		if !strings.HasPrefix(got, want) || strings.Contains(got, "%!") {
			t.Errorf("prompt %q sent %q, want prefix %q and no format error", prompt, got, want)
		}
	}
}

func TestOutputRetriesConfig(t *testing.T) {
	for _, tt := range []struct {
		name    string
		retries int
		calls   int
	}{
		{"one", 1, 2},
		{"none", NoOutputRetries, 1},
		{"default", 0, DefaultOutputRetries + 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, final := runOutput(t, Config{OutputRetries: tt.retries},
				llmtest.Turn{Text: "a"}, llmtest.Turn{Text: "b"}, llmtest.Turn{Text: "c"}, llmtest.Turn{Text: "d"})
			if final.Reason != ReasonInvalidOutput {
				t.Fatalf("reason = %v, want invalid_output", final.Reason)
			}
			if p.Calls() != tt.calls {
				t.Fatalf("calls = %d, want %d", p.Calls(), tt.calls)
			}
		})
	}
}
