package rubric

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

type deciderFunc func(ctx context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error)

func (f deciderFunc) Decide(ctx context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	return f(ctx, req)
}

// yesValues returns a decider answering c1, c2, ... with the given Yes values.
func yesValues(ys ...float64) deciderFunc {
	return func(_ context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
		answers := map[string]llm.Answer{}
		for i, y := range ys {
			answers[req.Questions[i].Name] = llm.Answer{Type: llm.QuestionYesNo, Yes: y}
		}
		return &llm.DecisionResponse{Answers: answers}, nil
	}
}

func refusing(_ context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	return &llm.DecisionResponse{Answers: map[string]llm.Answer{
		"c1": {Type: llm.QuestionYesNo, Yes: 1},
		"c2": {Type: llm.QuestionYesNo, Refused: true},
	}}, nil
}

var decisionReq = GradeRequest{Task: "t", Answer: "a", Criteria: []string{"first", "second", "third"}}

func TestDecisionGraderAllPass(t *testing.T) {
	g := DecisionGrader{Decider: yesValues(0.9, 0.5, 1)}
	got, err := g.Grade(t.Context(), decisionReq)
	if err != nil || got != (Grade{Pass: true}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestDecisionGraderFeedbackListsFailed(t *testing.T) {
	g := DecisionGrader{Decider: yesValues(0.9, 0.2, 0.7), Threshold: 0.6}
	got, err := g.Grade(t.Context(), decisionReq)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pass || got.Feedback != "Criteria not met:\n2. second\n" {
		t.Fatalf("got %+v", got)
	}
}

func TestDecisionGraderRequestShape(t *testing.T) {
	var seen llm.DecisionRequest
	d := deciderFunc(func(ctx context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
		seen = req
		return yesValues(1, 1, 1)(ctx, req)
	})
	if _, err := (DecisionGrader{Decider: d, Model: "m"}).Grade(t.Context(), decisionReq); err != nil {
		t.Fatal(err)
	}
	if seen.Model != "m" || len(seen.Questions) != 3 {
		t.Fatalf("request = %+v", seen)
	}
	for i, q := range seen.Questions {
		want := []string{"c1", "c2", "c3"}[i]
		if q.Name != want || q.Type != llm.QuestionYesNo || !strings.HasSuffix(q.Instructions, decisionReq.Criteria[i]) ||
			!strings.HasPrefix(q.Instructions, "Does the answer inside <answer> fully meet this criterion? ") {
			t.Errorf("question %d = %+v", i, q)
		}
	}
	if len(seen.Input) != 1 || !strings.Contains(seen.Input[0].Text, "<answer>") {
		t.Fatalf("input = %+v", seen.Input)
	}
}

func TestDecisionGraderErrors(t *testing.T) {
	boom := errors.New("boom")
	missing := deciderFunc(func(context.Context, llm.DecisionRequest) (*llm.DecisionResponse, error) {
		return &llm.DecisionResponse{Answers: map[string]llm.Answer{"c1": {Type: llm.QuestionYesNo, Yes: 1}, "c3": {Type: llm.QuestionYesNo, Yes: 1}}}, nil
	})
	answering := func(a llm.Answer) deciderFunc {
		return func(_ context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
			answers := map[string]llm.Answer{}
			for _, q := range req.Questions {
				answers[q.Name] = llm.Answer{Type: llm.QuestionYesNo, Yes: 1}
			}
			answers["c2"] = a
			return &llm.DecisionResponse{Answers: answers}, nil
		}
	}
	const invalid = "rubric: decider gave no valid yes/no answer for criterion 2"
	tests := map[string]struct {
		g    DecisionGrader
		want string
	}{
		"wrong type":   {DecisionGrader{Decider: answering(llm.Answer{Type: llm.QuestionChoice, Choice: "a"})}, invalid},
		"zero type":    {DecisionGrader{Decider: answering(llm.Answer{Yes: 1})}, invalid},
		"yes above 1":  {DecisionGrader{Decider: answering(llm.Answer{Type: llm.QuestionYesNo, Yes: 1.5})}, invalid},
		"yes negative": {DecisionGrader{Decider: answering(llm.Answer{Type: llm.QuestionYesNo, Yes: -0.1})}, invalid},
		"yes NaN":      {DecisionGrader{Decider: answering(llm.Answer{Type: llm.QuestionYesNo, Yes: math.NaN()})}, invalid},
		"threshold above 1": {DecisionGrader{Decider: yesValues(1, 1, 1), Threshold: 1.5},
			"rubric: DecisionGrader Threshold must be between 0 and 1"},
		"threshold NaN": {DecisionGrader{Decider: yesValues(1, 1, 1), Threshold: math.NaN()},
			"rubric: DecisionGrader Threshold must be between 0 and 1"},
		"threshold negative": {DecisionGrader{Decider: yesValues(1, 1, 1), Threshold: -0.5},
			"rubric: DecisionGrader Threshold must be between 0 and 1"},
		"nil decider": {DecisionGrader{}, "rubric: DecisionGrader needs a Decider"},
		"refused":     {DecisionGrader{Decider: deciderFunc(refusing)}, "rubric: decider refused criterion 2"},
		"missing":     {DecisionGrader{Decider: missing}, "rubric: decider gave no answer for criterion 2"},
		"decide": {DecisionGrader{Decider: deciderFunc(func(context.Context, llm.DecisionRequest) (*llm.DecisionResponse, error) {
			return nil, boom
		})}, "boom"},
	}
	for name, tt := range tests {
		_, err := tt.g.Grade(t.Context(), decisionReq)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tt.want)
		}
	}
}

func TestDecisionGraderTimeout(t *testing.T) {
	d := deciderFunc(func(ctx context.Context, _ llm.DecisionRequest) (*llm.DecisionResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	_, err := DecisionGrader{Decider: d, Timeout: 10 * time.Millisecond}.Grade(t.Context(), decisionReq)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestRubricWithDecisionGraderFailsOpen(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "answer"})
	g := DecisionGrader{Decider: deciderFunc(refusing)}
	a := newAgent(t, p, nil, mustNew(t, Config{Criteria: []string{"one", "two"}, Grader: g}))
	final := finalOf(a, input("task"))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason %s", final.Reason)
	}
	got := Grades(final.State)
	if len(got) != 1 || got[0].Error == "" || got[0].Pass {
		t.Fatalf("grades = %+v", got)
	}
}
