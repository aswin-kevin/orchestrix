package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/middleware/askuser"
)

// twoQuestions is a single-choice question followed by a multi-choice one.
var twoQuestions = []askuser.Question{
	{Question: "Which database?", Header: "DB", Options: []askuser.Choice{{Label: "Alpha", Description: "the old one"}, {Label: "Beta"}}},
	{Question: "Which targets?", Options: []askuser.Choice{{Label: "linux"}, {Label: "darwin"}, {Label: "windows"}}, MultiSelect: true},
}

func TestAskSelectsNumberedOptions(t *testing.T) {
	out := &promptLog{}
	ap := activeApprover(newLineReader(strings.NewReader("2\n1, 3\n")), out)

	answers, err := ap.Ask(context.Background(), twoQuestions)

	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 {
		t.Fatalf("got %d answers, want 2", len(answers))
	}
	if got := answers[0].Selected; !slices.Equal(got, []string{"Beta"}) {
		t.Errorf("first answer selected %v, want [Beta]", got)
	}
	if got := answers[1].Selected; !slices.Equal(got, []string{"linux", "windows"}) {
		t.Errorf("second answer selected %v, want [linux windows]", got)
	}
	if answers[0].Question != "Which database?" {
		t.Errorf("first answer names question %q", answers[0].Question)
	}
	shown := out.buf.String()
	for _, want := range []string{"[DB] Which database?", "1. Alpha - the old one", "2. Beta", "Which targets? [multi]", "3. windows"} {
		if !strings.Contains(shown, want) {
			t.Errorf("output = %q, want it to contain %q", shown, want)
		}
	}
	if strings.Contains(shown, "Which database? [multi]") {
		t.Errorf("single-choice question marked multi: %q", shown)
	}
}

func TestAskFreeText(t *testing.T) {
	// "1, 2" names two options of a single-choice question, so it is the
	// user's own words too.
	for _, line := range []string{"use sqlite instead", "7", "1, x", "1, 2"} {
		t.Run(line, func(t *testing.T) {
			ap := activeApprover(newLineReader(strings.NewReader(line+"\n")), &promptLog{})

			answers, err := ap.Ask(context.Background(), twoQuestions[:1])

			if err != nil {
				t.Fatal(err)
			}
			if a := answers[0]; a.Other != line || len(a.Selected) != 0 {
				t.Errorf("answer = %+v, want other %q", a, line)
			}
		})
	}
}

func TestAskEndOfInputFails(t *testing.T) {
	ap := activeApprover(newLineReader(strings.NewReader("1\n")), &promptLog{})

	_, err := ap.Ask(context.Background(), twoQuestions)

	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want end of input", err)
	}
}

func TestAskCancelled(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ap := activeApprover(newLineReader(pr), &promptLog{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ap.Ask(ctx, twoQuestions); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestAskEscapesModelText(t *testing.T) {
	out := &promptLog{}
	ap := activeApprover(newLineReader(strings.NewReader("1\n")), out)
	qs := []askuser.Question{{
		Question: "Pick\x1b[2J one",
		Header:   "H\x1b",
		Options:  []askuser.Choice{{Label: "A\x07", Description: "d\u009b"}, {Label: "B\r"}},
	}}

	if _, err := ap.Ask(context.Background(), qs); err != nil {
		t.Fatal(err)
	}

	shown := out.buf.String()
	if strings.ContainsAny(shown, "\x1b\x07\r\u009b") {
		t.Errorf("output contains raw control characters: %q", shown)
	}
	for _, want := range []string{`Pick\x1b[2J one`, `[H\x1b]`, `A\x07 - d\u009b`, `B\r`} {
		if !strings.Contains(shown, want) {
			t.Errorf("output = %q, want it to contain %q", shown, want)
		}
	}
}

func TestAskPromptMatchesQuestionKind(t *testing.T) {
	out := &promptLog{}
	ap := activeApprover(newLineReader(strings.NewReader("1\n1\n")), out)

	if _, err := ap.Ask(context.Background(), twoQuestions); err != nil {
		t.Fatal(err)
	}

	shown := out.buf.String()
	single, multi, ok := strings.Cut(shown, "Which targets?")
	if !ok {
		t.Fatalf("output = %q, want both questions", shown)
	}
	if !strings.Contains(single, singleAnswerPrompt) || strings.Contains(single, multiAnswerPrompt) {
		t.Errorf("single-choice prompt = %q, want %q", single, singleAnswerPrompt)
	}
	if !strings.Contains(multi, multiAnswerPrompt) {
		t.Errorf("multi-choice prompt = %q, want %q", multi, multiAnswerPrompt)
	}
}

func TestQuestionsWhileIdleFailWithoutPrompt(t *testing.T) {
	out := &promptLog{}
	// no turn runs
	ap := newApprover(newLineReader(strings.NewReader("1\n")), out)

	_, err := ap.Ask(context.Background(), []askuser.Question{{Question: "Which?", Options: []askuser.Choice{{Label: "a"}, {Label: "b"}}}})

	if err == nil || err.Error() != deniedNoTerminal {
		t.Errorf("err = %v, want %q", err, deniedNoTerminal)
	}
	if out.buf.Len() != 0 {
		t.Errorf("printed %q while idle, want nothing", out.buf.String())
	}
}
