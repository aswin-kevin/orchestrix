// Package rubric adds a self-grading loop. When the agent is about to finish,
// a Grader scores the answer against the configured criteria; if they are not
// all met, the run continues with the grader's feedback as a user message, so
// the model revises its answer, up to MaxIterations times.
//
// Only top-level runs (agent.RunInfo.Depth 0) are graded; subagents that
// inherit the stack are not. Grading is advisory and fails open: when the
// grader returns an error the run completes and the error is recorded in
// Grades. The answer is model output and may be untrusted, so before a
// ModelGrader sees it, it is framed in tags and any tag in it that could
// close or open such a frame is escaped; the grader's feedback is framed as
// <rubric_feedback>, escaped the same way, when sent back.
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
// DecisionGrader is a Grader backed by a decision model: one yes/no question
// per criterion, answered in a single call, instead of a free-form LLM reply.
//
// Feedback is a BeforeStop continuation, so it shares the run's
// agent.MaxStopContinuations slots with every other BeforeStop middleware;
// BeforeStop middleware later in the stack (the async reminder) are
// consulted first, earlier ones (hooks Stop hooks) only when rubric lets the
// answer through. Once the slots are spent, the run ends without grading
// its last answer.
package rubric
