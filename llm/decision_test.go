package llm

import (
	"strings"
	"testing"
)

func validDecisionRequest() DecisionRequest {
	return DecisionRequest{
		Input: []Block{{Type: BlockText, Text: "hello"}},
		Questions: []Question{
			{Name: "ok", Type: YesNo, Instructions: "Is it fine?"},
			{Name: "team", Type: Choice, Instructions: "Which team?", Options: []Option{{Value: "a"}, {Value: "b"}}},
			{Name: "quality", Type: Score, Instructions: "Rate it.", Options: []Option{{Value: "bad"}, {Value: "good"}}},
		},
	}
}

func TestDecisionRequestValidate(t *testing.T) {
	if err := validDecisionRequest().Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	levels := func(n int) []Option {
		o := make([]Option, n)
		for i := range o {
			o[i].Value = string(rune('a' + i))
		}
		return o
	}
	cases := []struct {
		name   string
		mutate func(r *DecisionRequest)
		want   string
	}{
		{"no questions", func(r *DecisionRequest) { r.Questions = nil }, "question"},
		{"no input", func(r *DecisionRequest) { r.Input = nil }, "input"},
		{"empty name", func(r *DecisionRequest) { r.Questions[0].Name = "" }, "name"},
		{"bad name", func(r *DecisionRequest) { r.Questions[0].Name = "has space" }, "has space"},
		{"long name", func(r *DecisionRequest) { r.Questions[0].Name = strings.Repeat("x", 65) }, "name"},
		{"duplicate name", func(r *DecisionRequest) { r.Questions[1].Name = "ok" }, "ok"},
		{"unknown type", func(r *DecisionRequest) { r.Questions[0].Type = "bogus" }, "ok"},
		{"blank instructions", func(r *DecisionRequest) { r.Questions[0].Instructions = " \n" }, "ok"},
		{"yes_no with options", func(r *DecisionRequest) { r.Questions[0].Options = levels(2) }, "ok"},
		{"choice one option", func(r *DecisionRequest) { r.Questions[1].Options = levels(1) }, "team"},
		{"score one level", func(r *DecisionRequest) { r.Questions[2].Options = levels(1) }, "quality"},
		{"score eleven levels", func(r *DecisionRequest) { r.Questions[2].Options = levels(11) }, "quality"},
		{"empty option value", func(r *DecisionRequest) { r.Questions[1].Options[0].Value = "" }, "team"},
		{"duplicate option value", func(r *DecisionRequest) { r.Questions[1].Options[1].Value = "a" }, "team"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validDecisionRequest()
			tc.mutate(&r)
			err := r.Validate()
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.HasPrefix(err.Error(), "llm: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q: want llm: prefix and %q", err, tc.want)
			}
		})
	}
	r := validDecisionRequest()
	r.Questions[2].Options = levels(10)
	if err := r.Validate(); err != nil {
		t.Fatalf("ten levels: %v", err)
	}
}

func TestCheckAnswered(t *testing.T) {
	qs := []Question{{Name: "ok", Type: YesNo}, {Name: "team", Type: Choice}}
	resp := &DecisionResponse{Answers: map[string]Answer{"ok": {Type: YesNo, Yes: 0.9}}}
	if err := resp.checkAnswered(qs); err == nil || !strings.Contains(err.Error(), `"team"`) {
		t.Fatalf("missing answer: %v", err)
	}
	resp.Answers["team"] = Answer{Type: Score}
	if err := resp.checkAnswered(qs); err == nil || !strings.Contains(err.Error(), "team") {
		t.Fatalf("wrong type: %v", err)
	}
	resp.Answers["team"] = Answer{Type: Score, Refused: true}
	if err := resp.checkAnswered(qs); err != nil {
		t.Fatalf("refused answer: %v", err)
	}
	resp.Answers["team"] = Answer{Type: Choice, Choice: "a"}
	if err := resp.checkAnswered(qs); err != nil {
		t.Fatalf("complete: %v", err)
	}
}
