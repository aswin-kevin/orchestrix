package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/termsafe"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
)

const (
	// multiMarker follows a question that accepts several options.
	multiMarker = " [multi]"
	// singleAnswerPrompt and multiAnswerPrompt are printed before the answer
	// to a single-choice and a multi-choice question is read.
	singleAnswerPrompt = "Answer (option number, or your own words): "
	multiAnswerPrompt  = "Answer (option numbers, comma-separated, or your own words): "
)

// Ask implements askuser.Asker on the terminal. It prints each question with
// its numbered options and reads one line per question: an option number
// (comma-separated numbers for a multi-choice question) selects options,
// anything else is the user's own answer. It shares the approver's input and
// prompt lock, so its questions never interleave with approval prompts. End of
// input or cancellation fails the call, which the model sees as a tool error,
// and so does a question asked while no turn runs or still open when it ends
// (see approver).
//
// askuser already strips control characters from the questions; the text is
// still passed through termsafe.String, since it is written by the model.
func (a *approver) Ask(ctx context.Context, questions []askuser.Question) ([]askuser.Answer, error) {
	tctx, release, err := a.takeTerminal(ctx)
	if errors.Is(err, errTurnEnded) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("no answer from the user: %w", err)
	}
	defer release()
	label := askerLabel(agent.Info(ctx))
	answers := make([]askuser.Answer, 0, len(questions))
	for _, q := range questions {
		fmt.Fprint(a.out, formatQuestion(label, q))
		line, err := a.readAnswer(ctx, tctx)
		if errors.Is(err, errTurnEnded) {
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("no answer from the user: %w", err)
		}
		answers = append(answers, parseAnswer(q, strings.TrimSpace(line)))
	}
	return answers, nil
}

// formatQuestion renders q with its numbered options, then the answer
// prompt. label names the asking subagent, if any.
func formatQuestion(label string, q askuser.Question) string {
	var sb strings.Builder
	sb.WriteString(label)
	if q.Header != "" {
		sb.WriteString("[" + termsafe.String(q.Header) + "] ")
	}
	sb.WriteString(termsafe.String(q.Question))
	if q.MultiSelect {
		sb.WriteString(multiMarker)
	}
	sb.WriteString("\n")
	for i, o := range q.Options {
		fmt.Fprintf(&sb, "  %d. %s", i+1, termsafe.String(o.Label))
		if o.Description != "" {
			sb.WriteString(" - " + termsafe.String(o.Description))
		}
		sb.WriteString("\n")
	}
	if q.MultiSelect {
		sb.WriteString(multiAnswerPrompt)
	} else {
		sb.WriteString(singleAnswerPrompt)
	}
	return sb.String()
}

// parseAnswer reads line as option numbers of q when every comma-separated
// part is the number of an option and, unless q is multi-choice, there is
// just one; anything else is the user's own words. A blank line is an empty
// answer, which the model sees as no answer.
func parseAnswer(q askuser.Question, line string) askuser.Answer {
	ans := askuser.Answer{Question: q.Question}
	var selected []string
	for part := range strings.SplitSeq(line, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > len(q.Options) {
			ans.Other = line
			return ans
		}
		selected = append(selected, q.Options[n-1].Label)
	}
	if len(selected) > 1 && !q.MultiSelect {
		ans.Other = line
		return ans
	}
	ans.Selected = selected
	return ans
}
