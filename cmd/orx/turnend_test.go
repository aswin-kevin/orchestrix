package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/permission"
)

func TestStatusLineAfterOpenPromptStartsNewLine(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newRenderer(&out, &errOut)
	// A question left open when the run's final event arrives.
	fmt.Fprint(r.prompts(), singleAnswerPrompt)
	r.render(agent.Event{Kind: agent.KindFinal, Final: &agent.Final{Reason: agent.ReasonCompleted}})
	if got := errOut.String(); !strings.Contains(got, singleAnswerPrompt+"\ntokens:") {
		t.Fatalf("stderr = %q, want the usage line on a line of its own", got)
	}

	// Once answered (the terminal echoed the user's newline), no extra line.
	errOut.Reset()
	w := r.prompts()
	fmt.Fprint(w, planPrompt)
	w.(answerReader).answerRead()
	r.render(agent.Event{Kind: agent.KindFinal, Final: &agent.Final{Reason: agent.ReasonCompleted}})
	if got := errOut.String(); !strings.HasPrefix(got, planPrompt+"tokens:") {
		t.Fatalf("stderr = %q, want no blank line after an answered prompt", got)
	}
}

func TestPlanApprovalPendingAtTurnEndIsDenied(t *testing.T) {
	pr, pw := io.Pipe() // never answered
	defer pw.Close()
	out := &promptLog{}
	ap := newApprover(newLineReader(pr), out)
	_, end := ap.beginTurn()

	done := make(chan permission.Decision, 1)
	go func() { done <- ap.decide(context.Background(), planRequest("1. read"), "") }()
	waitFor(t, "the plan prompt", func() bool { return out.contains(planPrompt) })
	end()

	d := <-done
	if d.Behavior != permission.Deny || d.Message != deniedNoTerminal {
		t.Fatalf("decision = %+v, want deny %q", d, deniedNoTerminal)
	}
	if !out.contains(planPrompt + noAnswerTurnEnded) {
		t.Errorf("output = %q, want the open plan prompt closed with %q", out.buf.String(), noAnswerTurnEnded)
	}
}

func TestMultiQuestionAskCutAtTurnEnd(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	out := &promptLog{}
	ap := newApprover(newLineReader(pr), out)
	_, end := ap.beginTurn()
	questions := []askuser.Question{
		{Question: "First?", Options: []askuser.Choice{{Label: "a"}, {Label: "b"}}},
		{Question: "Second?", Options: []askuser.Choice{{Label: "c"}, {Label: "d"}}},
	}

	type result struct {
		answers []askuser.Answer
		err     error
	}
	done := make(chan result, 1)
	go func() {
		answers, err := ap.Ask(context.Background(), questions)
		done <- result{answers, err}
	}()
	waitFor(t, "the first question", func() bool { return out.contains("First?") })
	fmt.Fprintln(pw, "1")
	waitFor(t, "the second question", func() bool { return out.contains("Second?") })
	end()

	res := <-done
	if !errors.Is(res.err, errTurnEnded) || res.answers != nil {
		t.Fatalf("ask = %+v, %v; want no answers and %v, not a partial result", res.answers, res.err, errTurnEnded)
	}
	if !out.contains(noAnswerTurnEnded) {
		t.Errorf("output = %q, want the open question closed with %q", out.buf.String(), noAnswerTurnEnded)
	}
	// The terminal is free again for the next turn.
	if err := ap.lockPrompt(context.Background()); err != nil {
		t.Fatalf("terminal still held: %v", err)
	}
	ap.unlockPrompt()
}
