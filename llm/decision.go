package llm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// QuestionType selects the shape of the answer a question asks for.
type QuestionType string

// Question types.
const (
	YesNo  QuestionType = "yes_no"
	Choice QuestionType = "choice"
	Score  QuestionType = "score"
)

// Option is a choice option or a score level.
type Option struct {
	Value       string
	Description string
}

// Question is one thing the model is asked to decide about the input.
type Question struct {
	Name         string // unique in the request; [A-Za-z0-9_-], 1-64
	Type         QuestionType
	Instructions string
	Options      []Option // choice: options; score: levels, lowest first
}

// DecisionRequest asks a decision model to answer Questions about Input.
type DecisionRequest struct {
	Model     string  // "" means the Config's model
	Input     []Block // text, image, document blocks
	Questions []Question
}

// Answer is the reply to one question. Only the fields for its Type are set.
type Answer struct {
	Type          QuestionType
	Yes           float64            // YesNo: probability of yes
	Choice        string             // Choice: the chosen option
	Score         float64            // Score: expected level index, may be fractional
	Probabilities map[string]float64 // Choice/Score: by Option.Value
	Confidence    float64            // Choice/Score
	Refused       bool               // the provider declined this question
}

// DecisionResponse carries one Answer per question, keyed by Question.Name.
type DecisionResponse struct {
	Model   string
	Answers map[string]Answer
	Usage   Usage
}

// Decider answers decision requests. Adapters for specific providers
// implement it.
type Decider interface {
	Decide(ctx context.Context, req DecisionRequest) (*DecisionResponse, error)
}

var questionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Validate reports whether the request is well formed. It does not inspect
// the kinds of the input blocks; adapters do that.
func (r DecisionRequest) Validate() error {
	if len(r.Questions) == 0 {
		return fmt.Errorf("llm: decision request has no questions")
	}
	if len(r.Input) == 0 {
		return fmt.Errorf("llm: decision request has no input")
	}
	seen := make(map[string]bool, len(r.Questions))
	for _, q := range r.Questions {
		if !questionName.MatchString(q.Name) {
			return fmt.Errorf("llm: question name %q must match [A-Za-z0-9_-]{1,64}", q.Name)
		}
		if seen[q.Name] {
			return fmt.Errorf("llm: duplicate question name %q", q.Name)
		}
		seen[q.Name] = true
		if err := q.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (q Question) validate() error {
	if strings.TrimSpace(q.Instructions) == "" {
		return fmt.Errorf("llm: question %q has no instructions", q.Name)
	}
	n := len(q.Options)
	switch q.Type {
	case YesNo:
		if n != 0 {
			return fmt.Errorf("llm: yes_no question %q must not have options", q.Name)
		}
	case Choice:
		if n < 2 {
			return fmt.Errorf("llm: choice question %q needs at least 2 options", q.Name)
		}
	case Score:
		if n < 2 || n > 10 {
			return fmt.Errorf("llm: score question %q needs 2 to 10 levels", q.Name)
		}
	default:
		return fmt.Errorf("llm: question %q has unknown type %q", q.Name, q.Type)
	}
	values := make(map[string]bool, n)
	for _, o := range q.Options {
		if o.Value == "" {
			return fmt.Errorf("llm: question %q has an option with an empty value", q.Name)
		}
		if values[o.Value] {
			return fmt.Errorf("llm: question %q has duplicate option value %q", q.Name, o.Value)
		}
		values[o.Value] = true
	}
	return nil
}

// checkAnswered verifies that every question has an answer. A refused answer
// counts whatever its Type; otherwise the answer's Type must match.
func (r *DecisionResponse) checkAnswered(qs []Question) error {
	for _, q := range qs {
		a, ok := r.Answers[q.Name]
		if !ok || (!a.Refused && a.Type != q.Type) {
			return fmt.Errorf("llm: decision reply has no answer for question %q", q.Name)
		}
	}
	return nil
}
