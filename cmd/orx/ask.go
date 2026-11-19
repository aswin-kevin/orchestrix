package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
)

const (
	// multiMarker follows a question that accepts several options.
	multiMarker = " [multi]"
	// answerPrompt is printed before the answer to each question is read.
	answerPrompt = "Answer (option numbers, comma-separated, or your own words): "
)

// ask implements askuser.Config.Ask on the terminal. It prints each question
// with its numbered options and reads one line per question: a line of
// comma-separated option numbers selects those options, anything else is the
// user's own answer. It shares the approver's input and prompt lock, so its
// questions never interleave with approval prompts. End of input or
// cancellation fails the call, which the model sees as a tool error.
//
// askuser already strips control characters from the questions; the text is
// still passed through termSafe, since it is written by the model.
func (a *approver) ask(ctx context.Context, questions []askuser.Question) ([]askuser.Answer, error) {
	a.prompt.Lock()
	defer a.prompt.Unlock()
	label := askerLabel(agent.Info(ctx))
	answers := make([]askuser.Answer, 0, len(questions))
	for _, q := range questions {
		fmt.Fprint(a.out, formatQuestion(label, q))
		line, err := a.in.next(ctx)
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
		sb.WriteString("[" + termSafe(q.Header) + "] ")
	}
	sb.WriteString(termSafe(q.Question))
	if q.MultiSelect {
		sb.WriteString(multiMarker)
	}
	sb.WriteString("\n")
	for i, o := range q.Options {
		fmt.Fprintf(&sb, "  %d. %s", i+1, termSafe(o.Label))
		if o.Description != "" {
			sb.WriteString(" - " + termSafe(o.Description))
		}
		sb.WriteString("\n")
	}
	sb.WriteString(answerPrompt)
	return sb.String()
}

// parseAnswer reads line as option numbers of q when every comma-separated
// part is the number of an option, else as the user's own words.
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
	ans.Selected = selected
	return ans
}
