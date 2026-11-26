package llmtest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func TestScriptedDecider(t *testing.T) {
	errBoom := errors.New("boom")
	d := llmtest.NewDecider(
		llmtest.DecisionTurn{
			Model:   "dm",
			Answers: map[string]llm.Answer{"q": {Type: llm.QuestionYesNo, Yes: 0.9}},
			Usage:   llm.Usage{Input: 3},
		},
		llmtest.DecisionTurn{Err: errBoom},
	)
	ctx := context.Background()
	req := llm.DecisionRequest{Input: []llm.Block{{Type: llm.BlockText, Text: "x"}}}

	resp, err := d.Decide(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "dm" || resp.Answers["q"].Yes != 0.9 || resp.Usage.Input != 3 {
		t.Errorf("resp = %+v", resp)
	}
	if _, err := d.Decide(ctx, req); err != errBoom {
		t.Errorf("err = %v, want errBoom as is", err)
	}
	if _, err := d.Decide(ctx, req); !errors.Is(err, llmtest.ErrExhausted) {
		t.Errorf("err = %v, want ErrExhausted", err)
	}
	if len(d.Requests) != 3 || d.Calls() != 3 {
		t.Errorf("requests = %d, calls = %d, want 3", len(d.Requests), d.Calls())
	}
}

func TestScriptedDeciderRecordsClone(t *testing.T) {
	d := llmtest.NewDecider(llmtest.DecisionTurn{})
	req := llm.DecisionRequest{
		Input:     []llm.Block{{Type: llm.BlockText, Text: "a"}},
		Questions: []llm.Question{{Name: "q"}},
	}
	if _, err := d.Decide(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Input[0].Text = "changed"
	req.Questions[0].Name = "changed"
	if got := d.Requests[0]; got.Input[0].Text != "a" || got.Questions[0].Name != "q" {
		t.Errorf("recorded request mutated: %+v", got)
	}
}

func TestScriptedDeciderCancelledCtx(t *testing.T) {
	d := llmtest.NewDecider(llmtest.DecisionTurn{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Decide(ctx, llm.DecisionRequest{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if d.Calls() != 0 {
		t.Errorf("Calls = %d, want 0", d.Calls())
	}
}

func TestScriptedDeciderConcurrent(t *testing.T) {
	d := llmtest.NewDecider(make([]llmtest.DecisionTurn, 20)...)
	done := make(chan struct{})
	for range 20 {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = d.Decide(context.Background(), llm.DecisionRequest{})
		}()
	}
	for range 20 {
		<-done
	}
	if d.Calls() != 20 {
		t.Errorf("Calls = %d, want 20", d.Calls())
	}
}
