package llm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// QuestionType selects the shape of the answer a question asks for.
type QuestionType string

// Question types.
const (
	QuestionYesNo  QuestionType = "yes_no"
	QuestionChoice QuestionType = "choice"
	QuestionScore  QuestionType = "score"
)

// Choice is one choice of a choice question or one level of a score
// question.
type Choice struct {
	// Value is the choice's identifier, used as the answer and as a key of
	// Answer.Probabilities; unique within a question.
	Value string
	// Description explains to the model what the choice or level means.
	Description string
}

// Question is one thing the model is asked to decide about the input.
type Question struct {
	Name string // unique in the request; [A-Za-z0-9_-], 1-64
	// Type selects the shape of the answer: yes/no, choice or score.
	Type QuestionType
	// Instructions tell the model what to decide; required.
	Instructions string
	Choices      []Choice // choice: choices; score: levels, lowest first
}

// DecisionRequest asks a decision model to answer Questions about Input.
type DecisionRequest struct {
	Model string  // "" means the Config's model
	Input []Block // text, image, document blocks
	// Questions are the questions to answer about Input.
	Questions []Question
}

// Answer is the reply to one question. Only the fields for its Type are set.
type Answer struct {
	// Type is the type of the question this answers; it selects which of the
	// other fields are set.
	Type   QuestionType
	Yes    float64 // QuestionYesNo: probability of yes
	Choice string  // Choice: the chosen choice's Value
	// Score: expected level index, may be fractional
	Score         float64
	Probabilities map[string]float64 // Choice/Score: by Choice.Value
	Confidence    float64            // Choice/Score
	Refused       bool               // the provider declined this question
}

// DecisionResponse carries one Answer per question, keyed by Question.Name.
type DecisionResponse struct {
	// Model is the model that produced the answers.
	Model string
	// Answers holds the answer to each question, keyed by Question.Name.
	Answers map[string]Answer
	// Usage is the token usage of the call.
	Usage Usage
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
		return errors.New("llm: decision request has no questions")
	}
	if len(r.Input) == 0 {
		return errors.New("llm: decision request has no input")
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
	n := len(q.Choices)
	switch q.Type {
	case QuestionYesNo:
		if n != 0 {
			return fmt.Errorf("llm: yes_no question %q must not have choices", q.Name)
		}
	case QuestionChoice:
		if n < 2 {
			return fmt.Errorf("llm: choice question %q needs at least 2 choices", q.Name)
		}
	case QuestionScore:
		if n < 2 || n > 10 {
			return fmt.Errorf("llm: score question %q needs 2 to 10 levels", q.Name)
		}
	default:
		return fmt.Errorf("llm: question %q has unknown type %q", q.Name, q.Type)
	}
	values := make(map[string]bool, n)
	for _, o := range q.Choices {
		if o.Value == "" {
			return fmt.Errorf("llm: question %q has a choice with an empty value", q.Name)
		}
		if values[o.Value] {
			return fmt.Errorf("llm: question %q has duplicate choice value %q", q.Name, o.Value)
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
		switch {
		case !ok:
			return fmt.Errorf("llm: decision reply has no answer for question %q", q.Name)
		case !a.Refused && a.Type != q.Type:
			return fmt.Errorf("llm: decision reply has a %q answer for question %q, want %q", a.Type, q.Name, q.Type)
		}
	}
	return nil
}
