package main

import (
	"context"
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
// The agent may call decide concurrently: parallel subagents share the
// approver. A prompt and the answer typed to it must stay paired, so prompt
// is held from printing a question until its answer is read, and a prompt
// raised by a subagent names it.
type approver struct {
	in  *lineReader
	out io.Writer

	// prompt serializes the questions on the terminal; it is held while
	// waiting for input.
	prompt sync.Mutex

	mu sync.Mutex // guards always only, never held while waiting for input
	// always holds the tools the user allowed for the rest of the session.
	always map[string]bool
}

func newApprover(in *lineReader, out io.Writer) *approver {
	return &approver{in: in, out: out, always: make(map[string]bool)}
}

// decide implements permission.Approver. Anything other than "y" or "a",
// including end of input, denies the call; cancellation denies it as
// "cancelled". exit_plan_mode gets its own prompt: see decidePlan.
func (a *approver) decide(ctx context.Context, req permission.Request, _ string) permission.Decision {
	if req.Tool == planmode.ToolExit {
		return a.decidePlan(ctx, req)
	}
	remember := !req.NoRemember
	if remember && a.isAlways(req.Tool) {
		return permission.Allowed()
	}
	a.prompt.Lock()
	defer a.prompt.Unlock()
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
	answer, err := a.in.next(ctx)
	if ctx.Err() != nil {
		return permission.Denied(deniedCancelled)
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
	a.prompt.Lock()
	defer a.prompt.Unlock()
	if strings.HasSuffix(req.Subject, planmode.TruncationMarker) {
		fmt.Fprintf(a.out, "%sorx: warning: the plan was cut before it could be shown, so it is rejected unseen\n", askerLabel(agent.Info(ctx)))
		return permission.Denied(deniedPlanTooLong)
	}
	fmt.Fprintf(a.out, "%sPlan for approval:\n%s\n%s", askerLabel(agent.Info(ctx)), termSafe(req.Subject), planPrompt)
	answer, err := a.in.next(ctx)
	switch {
	case ctx.Err() != nil:
		return permission.Denied(deniedCancelled)
	case err != nil:
		return permission.Denied(deniedByUser)
	case strings.EqualFold(strings.TrimSpace(answer), "y"):
		return permission.Allowed()
	}
	fmt.Fprint(a.out, feedbackPrompt)
	feedback, err := a.in.next(ctx)
	if ctx.Err() != nil {
		return permission.Denied(deniedCancelled)
	}
	if feedback = strings.TrimSpace(feedback); err != nil || feedback == "" {
		return permission.Denied(deniedByUser)
	}
	return permission.Denied(feedback)
}

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
