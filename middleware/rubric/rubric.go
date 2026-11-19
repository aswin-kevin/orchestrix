// Package rubric adds a self-grading loop. When the agent is about to finish,
// a Grader scores the answer against the configured criteria; if they are not
// all met, the run continues with the grader's feedback as a user message, so
// the model revises its answer, up to MaxIterations times.
//
// Only top-level runs (agent.RunInfo.Depth 0) are graded; subagents that
// inherit the stack are not. Grading is advisory and fails open: when the
// grader returns an error the run completes and the error is recorded in
// Grades. The answer is model output and may be untrusted, so it is framed
// and neutralized (internal/promptsafe) before a ModelGrader sees it, and the
// grader's feedback is framed as <rubric_feedback> when sent back.
//
// What is graded: the text of the reply that would end the run
// (resp.Message), not earlier assistant messages; an answer split across
// turns (after a reminder or a max-token resume, say "see above") is graded
// by its last part only. The task is the run's input text, capped at
// DefaultMaxTaskBytes, and the feedback sent back is capped at
// DefaultMaxFeedbackBytes. MaxIterations counts per run: a run resumed
// after an interrupt (agent.Agent.ResumeWith) starts with no gradings, so
// it may revise MaxIterations times again.
//
// Cost and observability: a grader's calls are its own. A ModelGrader's
// tokens are not in the run's Final.Usage and do not count towards
// Config.TokenBudget, and each call is bounded by ModelGrader.Timeout. A
// grader error is not an event: it shows only as Record.Error in Grades.
//
// Feedback is a BeforeStop continuation, so it shares the run's
// agent.MaxStopContinuations slots with every other BeforeStop middleware;
// BeforeStop middleware later in the stack (the async reminder) are
// consulted first, earlier ones (hooks Stop hooks) only when rubric lets the
// answer through. Once the slots are spent, the run ends without grading
// its last answer.
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
	// GradesKey holds the gradings of the current run, oldest first.
	GradesKey = state.NewKey[[]Record]("rubric.grades", state.Private[[]Record]())
	// taskKey holds the run's task text, so a run without input (a resumed
	// one) is still graded against it, not against a stop continuation. It
	// persists with the thread until a run with user input replaces it.
	taskKey = state.NewKey[string]("rubric.task", state.Private[string]())
)

// Grades returns the gradings recorded in s by the current run.
func Grades(s *state.State) []Record {
	g, _ := GradesKey.Get(s)
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

type middleware struct {
	criteria []string
	grader   Grader
	maxIter  int
}

// New returns the rubric middleware. It fails on missing or empty criteria
// and on MaxIterations outside 0..agent.MaxStopContinuations.
func New(cfg Config) (agent.Middleware, error) {
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
	m := &middleware{criteria: append([]string(nil), cfg.Criteria...), grader: cfg.Grader, maxIter: cfg.MaxIterations}
	if m.grader == nil {
		m.grader = ModelGrader{}
	}
	if m.maxIter == 0 {
		m.maxIter = DefaultMaxIterations
	}
	return m, nil
}

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "rubric" }

// BeforeAgent clears the previous gradings and remembers the run's task.
func (m *middleware) BeforeAgent(ctx context.Context, s *state.State) error {
	info, ok := agent.Info(ctx)
	if !ok || info.Depth != 0 {
		return nil
	}
	GradesKey.Set(s, nil)
	if task := userText(info.Input); task != "" {
		taskKey.Set(s, task)
	}
	return nil
}

// BeforeStop grades the answer and, when it falls short and revisions are
// left, returns the feedback prompt that continues the run.
func (m *middleware) BeforeStop(ctx context.Context, s *state.State, resp *llm.Response) (string, error) {
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
	})
	rec := Record{Iteration: iter, Pass: grade.Pass, Feedback: grade.Feedback}
	if err != nil {
		rec = Record{Iteration: iter, Error: err.Error()}
	}
	GradesKey.Set(s, append(records[:iter:iter], rec))
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
