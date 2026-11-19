package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/permission"
)

// deniedByUser is the message the model sees when the user declines a call.
const deniedByUser = "denied by user"

// deniedCancelled is the message when the turn is cancelled at a prompt.
const deniedCancelled = "cancelled"

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
// "cancelled".
func (a *approver) decide(ctx context.Context, req permission.Request, _ string) permission.Decision {
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
// first path when it has no subject.
func describe(req permission.Request) string {
	switch {
	case req.Subject != "":
		return req.Tool + " " + req.Subject
	case len(req.Paths) > 0:
		return req.Tool + " " + req.Paths[0].Path
	}
	return req.Tool
}
