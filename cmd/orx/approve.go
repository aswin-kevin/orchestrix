package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aswin-kevin/orchestrix/permission"
)

// deniedByUser is the message the model sees when the user declines a call.
const deniedByUser = "denied by user"

// approver asks the user, on the terminal, whether a tool call may run.
//
// It reads answers from the same bufio.Reader the REPL reads prompts from;
// two readers on one stream would each swallow input meant for the other.
// Sharing is safe because the agent asks for approval on its own goroutine
// while the REPL loop is blocked ranging over the run's events, so the two
// never read at the same time.
type approver struct {
	in  *bufio.Reader
	out io.Writer

	mu sync.Mutex
	// always holds the tools the user allowed for the rest of the session.
	always map[string]bool
}

func newApprover(in *bufio.Reader, out io.Writer) *approver {
	return &approver{in: in, out: out, always: make(map[string]bool)}
}

// decide implements permission.Approver. Anything other than "y" or "a",
// including end of input, denies the call.
func (a *approver) decide(_ context.Context, req permission.Request, _ string) permission.Decision {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.always[req.Tool] {
		return permission.Allowed()
	}
	fmt.Fprintf(a.out, "Allow %s? [y/N/a] ", describe(req))
	answer, _ := a.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "a":
		a.always[req.Tool] = true
		return permission.Allowed()
	case "y":
		return permission.Allowed()
	}
	return permission.Denied(deniedByUser)
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
