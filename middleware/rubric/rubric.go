package rubric

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// DefaultMaxIterations is how many revisions follow the first answer when
// Config.MaxIterations is zero.
const DefaultMaxIterations = 2

// Record is the outcome of one grading.
type Record struct {
	// Iteration is 0 for the first answer, 1 for the first revision, and so on.
	Iteration int `json:"iteration"`
	// Pass reports that the grader found every criterion met.
	Pass bool `json:"pass"`
	// Feedback is the grader's feedback, if any.
	Feedback string `json:"feedback,omitempty"`
	// Error is the grader's error; the run was let through.
	Error string `json:"error,omitempty"`
}

var (
	// gradesKey holds the gradings of the current run, oldest first. Read it
	// with Grades.
	gradesKey = state.NewKey[[]Record]("rubric.grades", state.Private[[]Record]())
	// taskKey holds the run's task text, so a run without input (a resumed
	// one) is still graded against it, not against a stop continuation. It
	// persists with the thread until a run with user input replaces it.
	taskKey = state.NewKey[string]("rubric.task", state.Private[string]())
)

// Grades returns the gradings recorded in s by the current run.
func Grades(s *state.State) []Record {
	g, _ := gradesKey.Get(s)
	return g
}

// Config configures the rubric middleware.
type Config struct {
	// Criteria are the requirements an answer must meet; required, and
	// each must be non-empty.
	Criteria []string
	// Grader scores the answer; nil means ModelGrader{}.
	Grader Grader
	// MaxIterations is how many revisions may follow the first answer; 0
	// means DefaultMaxIterations. More than agent.MaxStopContinuations is
	// an error.
	MaxIterations int
}

// Middleware is the rubric middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type Middleware struct {
	criteria []string
	grader   Grader
	maxIter  int
}

// Name is the name of the rubric middleware.
const Name = "rubric"

// New returns the rubric middleware. It fails on missing or empty criteria
// and on MaxIterations outside 0..agent.MaxStopContinuations.
func New(cfg Config) (*Middleware, error) {
	if len(cfg.Criteria) == 0 {
		return nil, errors.New("rubric: Criteria is required")
	}
	for i, c := range cfg.Criteria {
		if strings.TrimSpace(c) == "" {
			return nil, fmt.Errorf("rubric: criterion %d is empty", i+1)
		}
	}
	if cfg.MaxIterations < 0 || cfg.MaxIterations > agent.MaxStopContinuations {
		return nil, fmt.Errorf("rubric: MaxIterations must be between 0 and %d", agent.MaxStopContinuations)
	}
	m := &Middleware{criteria: append([]string(nil), cfg.Criteria...), grader: cfg.Grader, maxIter: cfg.MaxIterations}
	if m.grader == nil {
		m.grader = ModelGrader{}
	}
	if m.maxIter == 0 {
		m.maxIter = DefaultMaxIterations
	}
	return m, nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// BeforeAgent clears the previous gradings and remembers the run's task.
func (m *Middleware) BeforeAgent(ctx context.Context, s *state.State) error {
	info, ok := agent.Info(ctx)
	if !ok || info.Depth != 0 {
		return nil
	}
	gradesKey.Set(s, nil)
	if task := userText(info.Input); task != "" {
		taskKey.Set(s, task)
	}
	return nil
}

// BeforeStop grades the answer and, when it falls short and revisions are
// left, returns the feedback prompt that continues the run.
func (m *Middleware) BeforeStop(ctx context.Context, s *state.State, resp *llm.Response) (string, error) {
	if info, ok := agent.Info(ctx); !ok || info.Depth != 0 {
		return "", nil
	}
	records := Grades(s)
	iter := len(records)
	grade, err := m.grader.Grade(ctx, GradeRequest{
		Task:      taskOf(s),
		Answer:    resp.Message.Text(),
		Criteria:  m.criteria,
		Iteration: iter,
		Messages:  s.Messages(), // already a copy of the slice
	})
	rec := Record{Iteration: iter, Pass: grade.Pass, Feedback: grade.Feedback}
	if err != nil {
		rec = Record{Iteration: iter, Error: err.Error()}
	}
	gradesKey.Set(s, append(records[:iter:iter], rec))
	if err != nil || grade.Pass || iter >= m.maxIter {
		return "", nil
	}
	feedback, _ := promptsafe.CapBytes(grade.Feedback, DefaultMaxFeedbackBytes)
	return "<rubric_feedback>\n" + neutralizeFeedback(feedback) + "\n</rubric_feedback>\n" + feedbackSuffix, nil
}

// neutralizeFeedback escapes the frame tag of the feedback in grader text.
var neutralizeFeedback = promptsafe.NewTagNeutralizer("rubric_feedback")

// taskOf returns the run's task: the text saved by BeforeAgent, else the last
// user message of the conversation that is not injected.
func taskOf(s *state.State) string {
	if t, ok := taskKey.Get(s); ok {
		return t
	}
	msgs := s.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == llm.RoleUser && m.Meta[agent.InjectedMetaKey] == "" {
			if t := m.Text(); t != "" {
				return t
			}
		}
	}
	return ""
}

// userText joins the text of the user messages in msgs.
func userText(msgs []llm.Message) string {
	var parts []string
	for _, m := range msgs {
		if t := m.Text(); m.Role == llm.RoleUser && t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}
