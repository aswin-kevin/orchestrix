package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/permission"
)

// deniedByUser is the message the model sees when the user declines a call.
const deniedByUser = "denied by user"

// deniedCancelled is the message when the turn is cancelled at a prompt.
const deniedCancelled = "cancelled"

// deniedPlanTooLong is the message when a plan reached the approver cut.
const deniedPlanTooLong = "plan too long to review"

// deniedNoTerminal is the message, and the ask_user error, for a request
// made while no turn runs: a background task asking after its turn ended.
const deniedNoTerminal = "no one is at the terminal to answer; the background task cannot ask now"

// noAnswerTurnEnded closes, on screen, a question the turn left unanswered.
const noAnswerTurnEnded = "(no answer: the turn ended)"

// errTurnEnded ends a wait for the terminal or for an answer when no turn
// runs or the turn ends first. Its text is deniedNoTerminal, which ask_user
// returns as is.
var errTurnEnded = errors.New(deniedNoTerminal)

// The prompts of the plan approval.
const (
	planPrompt     = "Approve plan? [y/N] "
	feedbackPrompt = "Feedback for the model (blank for none): "
)

// approver asks the user, on the terminal, whether a tool call may run.
//
// It takes answers from the same lineReader the REPL takes prompts from.
// That is safe because the agent asks for approval while the REPL loop is
// blocked ranging over the run's events, so the two never wait for a line at
// the same time. Waiting also ends when the context is cancelled (Ctrl-C), so
// a turn cannot get stuck at a prompt.
//
// Only a running turn owns the terminal (see beginTurn). A background task
// that asks while no turn runs is denied at once, without a prompt: at the
// idle prompt the user's next line is their next message, not an answer. A
// question still open when its turn ends is closed and denied the same way,
// so the REPL, which takes prompt while it reads that line, gets the
// terminal back at once.
//
// The agent may call decide concurrently: parallel subagents share the
// approver. A prompt and the answer typed to it must stay paired, so prompt
// is held from printing a question until its answer is read, and a prompt
// raised by a subagent names it.
type approver struct {
	in  *lineReader
	out io.Writer

	// prompt serializes the questions on the terminal; it is held while
	// waiting for input. It is a one-slot semaphore, so that waiting for it
	// can end with a context (see lockPrompt).
	prompt chan struct{}

	mu sync.Mutex // guards always and turn, never held while waiting for input
	// always holds the tools the user allowed for the rest of the session.
	always map[string]bool
	// turn is the context of the running turn, which owns the terminal; nil
	// between turns. It is cancelled when the turn ends.
	turn context.Context
}

func newApprover(in *lineReader, out io.Writer) *approver {
	return &approver{in: in, out: out, prompt: make(chan struct{}, 1), always: make(map[string]bool)}
}

// decide implements permission.Approver. Anything other than "y" or "a",
// including end of input, denies the call; cancellation denies it as
// "cancelled". exit_plan_mode gets its own prompt: see decidePlan.
func (a *approver) decide(ctx context.Context, req permission.Request, _ string) permission.Decision {
	if !a.turnActive() {
		return permission.Denied(deniedNoTerminal)
	}
	if req.Tool == planmode.ToolExit {
		return a.decidePlan(ctx, req)
	}
	remember := !req.NoRemember
	if remember && a.isAlways(req.Tool) {
		return permission.Allowed()
	}
	tctx, release, err := a.takeTerminal(ctx)
	if err != nil {
		return deniedFor(err)
	}
	defer release()
	// The question answered while this call waited may have been "a" for
	// the same tool.
	if remember && a.isAlways(req.Tool) {
		return permission.Allowed()
	}
	choices := "[y/N/a]"
	if !remember {
		choices = "[y/N]"
	}
	fmt.Fprintf(a.out, "%sAllow %s? %s ", askerLabel(agent.Info(ctx)), describe(req), choices)
	answer, err := a.readAnswer(ctx, tctx)
	if errors.Is(err, errTurnEnded) || ctx.Err() != nil {
		return deniedFor(err)
	}
	if err != nil {
		return permission.Denied(deniedByUser)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "a":
		if !remember {
			return permission.Denied(deniedByUser) // "a" is not offered here
		}
		a.allowAlways(req.Tool)
		return permission.Allowed()
	case "y":
		return permission.Allowed()
	}
	return permission.Denied(deniedByUser)
}

// decidePlan asks the user to approve the plan of an exit_plan_mode call: it
// prints the whole plan, then asks with [y/N]. An approval is never
// remembered, whatever the request says, so every plan is read before it is
// approved. Any answer but "y" rejects the plan and reads one more line, the
// feedback the model gets to revise it; a blank one or end of input sends
// deniedByUser instead. A plan that reached the approver cut (its Subject
// ends with planmode.TruncationMarker) cannot be reviewed, so it is rejected
// without asking.
func (a *approver) decidePlan(ctx context.Context, req permission.Request) permission.Decision {
	tctx, release, err := a.takeTerminal(ctx)
	if err != nil {
		return deniedFor(err)
	}
	defer release()
	if strings.HasSuffix(req.Subject, planmode.TruncationMarker) {
		fmt.Fprintf(a.out, "%sorx: warning: the plan was cut before it could be shown, so it is rejected unseen\n", askerLabel(agent.Info(ctx)))
		return permission.Denied(deniedPlanTooLong)
	}
	fmt.Fprintf(a.out, "%sPlan for approval:\n%s\n%s", askerLabel(agent.Info(ctx)), termSafe(req.Subject), planPrompt)
	answer, err := a.readAnswer(ctx, tctx)
	switch {
	case errors.Is(err, errTurnEnded) || ctx.Err() != nil:
		return deniedFor(err)
	case err != nil:
		return permission.Denied(deniedByUser)
	case strings.EqualFold(strings.TrimSpace(answer), "y"):
		return permission.Allowed()
	}
	fmt.Fprint(a.out, feedbackPrompt)
	feedback, err := a.readAnswer(ctx, tctx)
	if errors.Is(err, errTurnEnded) || ctx.Err() != nil {
		return deniedFor(err)
	}
	if feedback = strings.TrimSpace(feedback); err != nil || feedback == "" {
		return permission.Denied(deniedByUser)
	}
	return permission.Denied(feedback)
}

// beginTurn marks the terminal as owned by a running turn until end is
// called. The returned turn context is cancelled by end, which also ends
// any question still open on the terminal.
func (a *approver) beginTurn() (turn context.Context, end func()) {
	turn, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.turn = turn
	a.mu.Unlock()
	return turn, func() {
		a.mu.Lock()
		a.turn = nil
		a.mu.Unlock()
		cancel()
	}
}

// turnActive reports whether a turn owns the terminal.
func (a *approver) turnActive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turn != nil
}

// takeTerminal waits for the terminal to ask one question during the
// running turn. It returns the context to read answers under, which also
// ends when the turn ends, and release, which gives the terminal back. err
// is errTurnEnded when no turn runs or it ends first, else ctx's error.
func (a *approver) takeTerminal(ctx context.Context) (tctx context.Context, release func(), err error) {
	a.mu.Lock()
	turn := a.turn
	a.mu.Unlock()
	if turn == nil {
		return nil, nil, errTurnEnded
	}
	tctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(turn, cancel)
	if err := a.lockPrompt(tctx); err != nil {
		stop()
		cancel()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errTurnEnded
	}
	return tctx, func() {
		a.unlockPrompt()
		stop()
		cancel()
	}, nil
}

// readAnswer reads the answer to the question just printed. When the turn
// ends first (tctx ended but ctx did not), it closes the question on screen
// and returns errTurnEnded.
func (a *approver) readAnswer(ctx, tctx context.Context) (string, error) {
	line, err := a.in.next(tctx)
	if err != nil && ctx.Err() == nil && tctx.Err() != nil {
		fmt.Fprintln(a.out, noAnswerTurnEnded)
		return "", errTurnEnded
	}
	return line, err
}

// deniedFor is the denial of a question that got no answer because of err:
// the turn ended, or the call was cancelled.
func deniedFor(err error) permission.Decision {
	if errors.Is(err, errTurnEnded) {
		return permission.Denied(deniedNoTerminal)
	}
	return permission.Denied(deniedCancelled)
}

// lockPrompt takes the terminal for a question or the idle prompt; it gives
// up, returning ctx's error, when ctx ends first.
func (a *approver) lockPrompt(ctx context.Context) error {
	select {
	case a.prompt <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlockPrompt gives the terminal back.
func (a *approver) unlockPrompt() { <-a.prompt }

func (a *approver) isAlways(tool string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.always[tool]
}

func (a *approver) allowAlways(tool string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.always[tool] = true
}

// askerLabel prefixes the prompt of a subagent's call with the subagent's
// name, so the user knows which agent asks; a top-level run's prompt has no
// prefix.
func askerLabel(info agent.RunInfo, inRun bool) string {
	if !inRun || info.Depth == 0 {
		return ""
	}
	return "[" + info.AgentName + "] "
}

// describe names the call in a prompt: the tool plus its subject, or its
// first path when it has no subject. Every part may be written by the model
// (a command, a URL, a path, an MCP tool name), so it is passed through
// termSafe.
func describe(req permission.Request) string {
	switch {
	case req.Subject != "":
		return termSafe(req.Tool + " " + req.Subject)
	case len(req.Paths) > 0:
		return termSafe(req.Tool + " " + req.Paths[0].Path)
	}
	return termSafe(req.Tool)
}
