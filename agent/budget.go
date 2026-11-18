package agent

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
)

// Prompts and limits of the run budgets (Config.MaxTurns, MaxDuration,
// TokenBudget, EscalateMaxTokens and Settlement).
const (
	// EscalatedMaxTokens is the output limit a reply cut off at MaxTokens is
	// retried with when Config.EscalateMaxTokens is set, capped at the
	// profile's MaxOutput.
	EscalatedMaxTokens = 64_000
	// DefaultBudgetNudge is sent, with the number of tokens left, when the
	// model stops well short of Config.TokenBudget.
	DefaultBudgetNudge = "Keep working — about %d tokens of budget remain. Continue the task; stop only when it is genuinely complete."
	// DefaultResumePrompt asks the model to continue a reply cut off at the
	// output limit.
	DefaultResumePrompt = "Output token limit hit. Resume exactly where you left off — no apology, no recap."

	// maxResumeAttempts bounds how many truncated replies a run resumes
	// before it settles for what it has.
	maxResumeAttempts = 3
	// budgetNudgePercent is the share of Config.TokenBudget below which a
	// model that stops is nudged to keep going.
	budgetNudgePercent = 90
	// diminishingReturnsTokens is the output under which a nudge counts as
	// unproductive.
	diminishingReturnsTokens = 500
	// unproductiveNudgeLimit is how many unproductive nudges in a row stop
	// the nudging.
	unproductiveNudgeLimit = 2
)

// timeBudgetToolResult is the result of a call that Config.MaxDuration
// interrupted.
const timeBudgetToolResult = "Tool call interrupted: time budget exhausted."

// budget is the per-run bookkeeping behind the run limits. Its counters are
// touched only by the loop goroutine; the contexts never change, so tool
// goroutines may read them.
type budget struct {
	// toolCtx is the run context bounded by Config.MaxDuration. Only tool
	// calls run under it: a model call is allowed to finish its turn, and the
	// settlement after a timeout must still reach the model.
	toolCtx   context.Context
	runCtx    context.Context
	stopClock context.CancelFunc

	// toolTurns counts model calls that produced tool calls, the unit of
	// Config.MaxTurns.
	toolTurns int

	// maxTokens replaces the request's MaxTokens once escalated; zero until
	// then.
	maxTokens int
	// resumes counts the truncated replies resumed so far.
	resumes int

	// nudged reports that the model was last stopped by a budget nudge, sent
	// when the run's output was outputAtNudge.
	nudged        bool
	outputAtNudge int
	// unproductiveNudges counts consecutive nudges that yielded little.
	unproductiveNudges int

	// settleReason is the limit being settled; empty outside settlement.
	settleReason TerminalReason
	// settleTurnsLeft is how many settlement model calls remain.
	settleTurnsLeft int
}

// newBudget starts the run's clock. Call stopClock once the run is over.
func newBudget(ctx context.Context, maxDuration time.Duration) *budget {
	b := &budget{toolCtx: ctx, runCtx: ctx, stopClock: func() {}}
	if maxDuration > 0 {
		b.toolCtx, b.stopClock = context.WithTimeout(ctx, maxDuration)
	}
	return b
}

// expired reports whether Config.MaxDuration has run out. A run its caller
// cancelled is not expired, so the two can be told apart.
func (b *budget) expired() bool {
	return b.toolCtx.Err() != nil && b.runCtx.Err() == nil
}

func (b *budget) settling() bool {
	return b.settleReason != ""
}

// nudge returns the prompt that keeps a model going when it stopped with
// output tokens spent out of limit, or false when the run should end: the
// budget is (nearly) spent, or nudging stopped paying off.
func (b *budget) nudge(limit, output int) (string, bool) {
	if b.nudged && output-b.outputAtNudge < diminishingReturnsTokens {
		b.unproductiveNudges++
	} else {
		b.unproductiveNudges = 0
	}
	b.nudged = false
	if limit <= 0 || output*100 >= limit*budgetNudgePercent || b.unproductiveNudges >= unproductiveNudgeLimit {
		return "", false
	}
	b.nudged, b.outputAtNudge = true, output
	return fmt.Sprintf(DefaultBudgetNudge, limit-output), true
}

// escalate handles a tool-less reply cut off at the output limit by retrying
// the same turn with a larger limit, once per run. It reports whether resp
// was discarded for that retry.
func (r *run) escalate(resp *llm.Response) bool {
	b := r.budget
	if !r.agent.cfg.EscalateMaxTokens || b.maxTokens > 0 || b.settling() || b.expired() ||
		resp.StopReason != llm.StopMaxTokens || len(resp.Message.ToolUses()) > 0 {
		return false
	}
	limit := r.escalatedMaxTokens()
	if limit <= r.maxTokens() {
		return false
	}
	b.maxTokens = limit
	// The consumer has seen the truncated text and must discard it.
	r.emit(Event{Kind: KindRetry})
	return true
}

func (r *run) escalatedMaxTokens() int {
	if profileMax := r.agent.cfg.Profile.MaxOutput; profileMax > 0 {
		return min(EscalatedMaxTokens, profileMax)
	}
	return EscalatedMaxTokens
}

// maxTokens is the output limit of the next model request.
func (r *run) maxTokens() int {
	cfg := r.agent.cfg
	switch {
	case r.budget.maxTokens > 0:
		return r.budget.maxTokens
	case cfg.MaxTokens > 0:
		return cfg.MaxTokens
	default:
		return cfg.Profile.MaxOutput
	}
}

// afterToolTurn applies the run limits once a turn's tool calls are done. It
// returns a Final when a limit ends the run.
func (r *run) afterToolTurn() *Final {
	b := r.budget
	if b.settling() {
		b.settleTurnsLeft--
		if b.settleTurnsLeft <= 0 {
			return r.finish(b.settleReason, nil)
		}
		return nil
	}
	b.toolTurns++
	switch maxTurns := r.agent.cfg.MaxTurns; {
	case b.expired():
		return r.limitReached(ReasonTimeout)
	case maxTurns > 0 && b.toolTurns >= maxTurns:
		return r.limitReached(ReasonMaxTurns)
	}
	return nil
}

// afterNoToolTurn decides what follows a reply without tool calls: resuming
// a truncated reply, nudging the model toward its token budget, or the end
// of the run.
func (r *run) afterNoToolTurn(resp *llm.Response) *Final {
	b := r.budget
	if b.settling() {
		// Settlement never ends a run as completed: the limit still applies.
		return r.finish(b.settleReason, nil)
	}
	if resp.StopReason == llm.StopMaxTokens {
		if b.resumes >= maxResumeAttempts {
			return r.finish(ReasonCompleted, nil)
		}
		b.resumes++
		return r.continueWith(DefaultResumePrompt)
	}
	if prompt, ok := b.nudge(r.agent.cfg.TokenBudget, r.usage.Output); ok {
		return r.continueWith(prompt)
	}
	return r.finish(ReasonCompleted, nil)
}

// continueWith appends prompt as a user message so the model takes another
// turn, unless the time budget has run out.
func (r *run) continueWith(prompt string) *Final {
	if r.budget.expired() {
		return r.limitReached(ReasonTimeout)
	}
	r.state.AppendMessages(llm.UserText(prompt))
	return nil
}

// limitReached ends the run with reason, or first enters the settlement
// phase when one is configured for reason.
func (r *run) limitReached(reason TerminalReason) *Final {
	prompt := r.settlementPrompt(reason)
	if prompt == "" {
		return r.finish(reason, nil)
	}
	r.budget.settleReason = reason
	r.budget.settleTurnsLeft = r.agent.cfg.Settlement.MaxTurns
	r.state.AppendMessages(llm.UserText(prompt))
	return nil
}

// settlementPrompt returns the settlement prompt for reason, empty when the
// run has no settlement phase.
func (r *run) settlementPrompt(reason TerminalReason) string {
	s := r.agent.cfg.Settlement
	if s == nil {
		return ""
	}
	if prompt := s.PromptByReason[reason]; prompt != "" {
		return prompt
	}
	return s.Prompt
}

// toolSchemas lists the tools offered to the model, which leaves out the
// disabled ones during settlement.
func (r *run) toolSchemas() []llm.ToolSchema {
	return slices.DeleteFunc(r.agent.registry.Schemas(), func(s llm.ToolSchema) bool {
		return r.disabled(s.Name)
	})
}

// disabled reports whether the named tool is withheld right now.
func (r *run) disabled(name string) bool {
	return r.budget.settling() && slices.Contains(r.agent.cfg.Settlement.DisabledTools, name)
}
