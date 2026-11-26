package rubric

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/llm"
)

// Defaults applied by ModelGrader and Config when the field is zero.
const (
	// DefaultMaxAnswerBytes caps, in bytes, the answer text sent to a model
	// grader.
	DefaultMaxAnswerBytes = 20_000
	// DefaultMaxTaskBytes caps, in bytes, the task text sent to a model
	// grader.
	DefaultMaxTaskBytes = 20_000
	// DefaultMaxFeedbackBytes caps, in bytes, the grader feedback sent back
	// to the model, whatever Grader produced it.
	DefaultMaxFeedbackBytes = 4_000
	// DefaultGradeTimeout bounds one ModelGrader call when its Timeout is
	// zero.
	DefaultGradeTimeout = 2 * time.Minute
	// DefaultGradeMaxTokens is the output limit of a model grader call.
	DefaultGradeMaxTokens = 1_024
)

// GradeRequest is what a Grader judges.
type GradeRequest struct {
	// Task is the user's request the answer responds to; may be empty when
	// it cannot be found.
	Task string
	// Answer is the model's reply being graded.
	Answer string
	// Criteria are the configured criteria, in order.
	Criteria []string
	// Iteration counts the earlier gradings of this run: 0 for the first
	// answer.
	Iteration int
	// Messages is the run's full history in State when the answer was
	// given, oldest first, the answer's assistant message last: not the
	// view the model was sent, which summarize or compress may have
	// shortened. The slice is the Grader's own, but the messages' content
	// is shared with the State and must be treated as read-only.
	// ModelGrader and DecisionGrader ignore it; a custom Grader may use it
	// to judge the answer against the whole conversation.
	Messages []llm.Message
}

// Grade is a grader's verdict.
type Grade struct {
	// Pass reports that every criterion is met.
	Pass bool `json:"pass"`
	// Feedback says what to fix when Pass is false; may be empty.
	Feedback string `json:"feedback"`
}

// Grader scores an answer against criteria. An error makes the middleware
// let the run complete (see New).
type Grader interface {
	Grade(ctx context.Context, req GradeRequest) (Grade, error)
}

// GraderFunc adapts a function to a Grader.
type GraderFunc func(ctx context.Context, req GradeRequest) (Grade, error)

// Grade calls f.
func (f GraderFunc) Grade(ctx context.Context, req GradeRequest) (Grade, error) {
	return f(ctx, req)
}

// ModelGrader grades by asking a model. The zero value uses the running
// agent's provider and model, so it only works inside a run.
type ModelGrader struct {
	// Provider answers the grading request; nil means the run's own.
	Provider llm.Provider
	// Model is sent as the request model; "" means the run's own, or, with
	// a Provider set, the provider's default.
	Model string
	// Prompt is the system prompt; "" means DefaultGraderPrompt.
	Prompt string
	// Timeout bounds each grading call; 0 means DefaultGradeTimeout. A call
	// that runs out fails, and the run completes (grading fails open).
	Timeout time.Duration
}

// neutralize escapes the frame tags of the grading request in untrusted text.
var neutralize = promptsafe.NewTagNeutralizer("task", "criteria", "answer")

// Grade implements Grader.
func (g ModelGrader) Grade(ctx context.Context, req GradeRequest) (Grade, error) {
	provider, model := g.Provider, g.Model
	if provider == nil {
		info, ok := agent.Info(ctx)
		if !ok {
			return Grade{}, errors.New("rubric: ModelGrader without a Provider needs a run context")
		}
		cfg := info.Agent.Config()
		provider = cfg.Provider
		if model == "" {
			model = cfg.Model
		}
	}
	system := g.Prompt
	if system == "" {
		system = DefaultGraderPrompt
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultGradeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := provider.Complete(ctx, llm.Request{
		Model:     model,
		System:    []string{system},
		Messages:  []llm.Message{llm.UserText(gradingMessage(req))},
		MaxTokens: DefaultGradeMaxTokens,
	})
	if err != nil {
		return Grade{}, err
	}
	return parseGrade(resp.Message.Text())
}

// gradingMessage frames the task, criteria and answer for the grader.
func gradingMessage(req GradeRequest) string {
	answer, _ := promptsafe.CapBytes(req.Answer, DefaultMaxAnswerBytes)
	task, _ := promptsafe.CapBytes(req.Task, DefaultMaxTaskBytes)
	var sb strings.Builder
	sb.WriteString("<task>\n" + neutralize(task) + "\n</task>\n<criteria>\n")
	for i, c := range req.Criteria {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, neutralize(c))
	}
	sb.WriteString("</criteria>\n<answer>\n" + neutralize(answer) + "\n</answer>")
	return sb.String()
}

// parseGrade reads the grade from the first "{" to the last "}" of text.
func parseGrade(text string) (Grade, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return Grade{}, errors.New("rubric: grader reply has no JSON object")
	}
	var g Grade
	if err := json.Unmarshal([]byte(text[start:end+1]), &g); err != nil {
		return Grade{}, fmt.Errorf("rubric: grader reply is not a valid grade: %w", err)
	}
	return g, nil
}
