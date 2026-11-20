package orchestrix_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

type verdict struct {
	Severity string   `json:"severity" enum:"low,high"`
	CVEs     []string `json:"cves"`
	Score    int      `json:"score,omitempty"`
}

func runVerdict(t *testing.T, p llm.Provider, opts ...orchestrix.Option) (verdict, *agent.Final, error) {
	t.Helper()
	opts = append([]orchestrix.Option{orchestrix.WithOutputType[verdict]()}, opts...)
	a, err := orchestrix.NewWithProvider(p, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return orchestrix.RunTyped[verdict](context.Background(), a,
		agent.Input{Messages: []llm.Message{llm.UserText("assess")}})
}

func TestWithOutputTypeEndToEnd(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: `{"severity":"high","cves":["CVE-2026-1"]}`})
	got, final, err := runVerdict(t, p)
	if err != nil {
		t.Fatal(err)
	}
	want := verdict{Severity: "high", CVEs: []string{"CVE-2026-1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Errorf("final = %+v, want completed", final)
	}
}

func TestRunTypedCorrects(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{Text: "thinking..."},
		llmtest.Turn{Text: `{"severity":"low","cves":[]}`},
	)
	got, _, err := runVerdict(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Severity != "low" {
		t.Errorf("got %+v", got)
	}
	if p.Calls() != 2 {
		t.Errorf("calls = %d, want 2", p.Calls())
	}
}

func TestDecodeErrors(t *testing.T) {
	if _, err := orchestrix.Decode[verdict](nil); err == nil {
		t.Error("nil final: want error")
	}
	_, err := orchestrix.Decode[verdict](&agent.Final{Reason: agent.ReasonMaxTurns})
	if err == nil || !strings.Contains(err.Error(), "max_turns") {
		t.Errorf("max_turns: err = %v", err)
	}
	_, err = orchestrix.Decode[verdict](&agent.Final{
		Reason: agent.ReasonInvalidOutput,
		Err:    errors.Join(agent.ErrInvalidOutput, errors.New("bad")),
	})
	if !errors.Is(err, agent.ErrInvalidOutput) {
		t.Errorf("invalid_output: err = %v", err)
	}
	_, err = orchestrix.Decode[verdict](&agent.Final{Reason: agent.ReasonCompleted})
	if err == nil || !strings.Contains(err.Error(), "no structured output") {
		t.Errorf("nil output: err = %v", err)
	}
	_, err = orchestrix.Decode[verdict](&agent.Final{
		Reason: agent.ReasonCompleted,
		Output: json.RawMessage(`{"severity":"low","cves":[],"extra":1}`),
	})
	if err == nil || !strings.Contains(err.Error(), "extra") {
		t.Errorf("unknown field: err = %v, want it to name the field", err)
	}
}

func TestRunTypedInvalidOutputKeepsFinal(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "a"}, llmtest.Turn{Text: "b"}, llmtest.Turn{Text: "c"})
	_, final, err := runVerdict(t, p)
	if !errors.Is(err, agent.ErrInvalidOutput) {
		t.Errorf("err = %v, want ErrInvalidOutput", err)
	}
	if err == nil || !strings.Contains(err.Error(), string(agent.ReasonInvalidOutput)) {
		t.Errorf("err = %v, want it to name the reason", err)
	}
	if final == nil {
		t.Fatal("final = nil, want the Final of the run")
	}
	if final.Text != "c" || final.Reason != agent.ReasonInvalidOutput {
		t.Errorf("final = %+v, want text c and reason invalid_output", final)
	}
}

func TestRunTypedModelErrorKeepsFinal(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Err: errors.New("model down")})
	_, final, err := runVerdict(t, p)
	if err == nil {
		t.Error("want error for a failed model call")
	}
	if final == nil {
		t.Fatal("final = nil, want the Final of the run")
	}
	if final.Reason == agent.ReasonCompleted {
		t.Errorf("final reason = %s, want a failure", final.Reason)
	}
}

func TestDecodeNormalizedNumbers(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: `{"severity":"low","cves":[],"score":1e2}`})
	got, _, err := runVerdict(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Score != 100 {
		t.Errorf("Score = %d, want 100", got.Score)
	}
}

func TestRunTypedInterrupted(t *testing.T) {
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{
		{ID: "c1", Name: "write_file", Input: map[string]any{"path": "a.txt", "content": "x"}},
	}})
	_, final, err := runVerdict(t, p, orchestrix.WithInterruptOn("write_file"))
	if err == nil {
		t.Error("want error for interrupted run")
	}
	if final == nil || final.Reason != agent.ReasonInterrupted {
		t.Errorf("final = %+v, want interrupted", final)
	}
}

func TestWithOutputSchemaRejectsArray(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.WithOutputSchema(map[string]any{"type": "array"}))
	if err == nil {
		t.Error("want error for a non-object schema")
	}
}
