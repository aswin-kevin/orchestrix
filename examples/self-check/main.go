// Command self-check grades the agent's answer against a rubric before the
// run ends; an answer that falls short goes back to the model with the
// grader's feedback until it passes or the revisions run out.
//
//	go run ./examples/self-check                                 # offline, scripted
//	ORX_MODEL=openai:gpt-4o go run ./examples/self-check
//
// Offline, a small Go function grades the answer. Live, the default model
// grader asks the same model to judge it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/examples/internal/exmodel"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "self-check:", exmodel.Safe(err.Error()))
		os.Exit(1)
	}
}

// criterion is the one requirement every answer is graded on.
const criterion = "The answer mentions units for every number."

// units are the unit names the offline grader accepts.
var units = []string{"km", "kilometers", "kilometres", "miles"}

// mentionsUnits is the offline grader: a plain Go check, standing in for
// the model grader so the example runs without a network.
var mentionsUnits = rubric.GraderFunc(func(_ context.Context, req rubric.GradeRequest) (rubric.Grade, error) {
	for _, u := range units {
		if strings.Contains(req.Answer, u) {
			return rubric.Grade{Pass: true}, nil
		}
	}
	return rubric.Grade{Pass: false, Feedback: "The distance has no unit; state it in kilometers or miles."}, nil
})

func run(ctx context.Context, w io.Writer) error {
	cfg := rubric.Config{Criteria: []string{criterion}, MaxIterations: 2}
	if !exmodel.Live() {
		cfg.Grader = mentionsUnits // nil (live) means rubric.ModelGrader{}
	}
	model := exmodel.Model(
		llmtest.Turn{Text: "The Moon is on average 384,400 away from Earth."}, // fails: no unit
		llmtest.Turn{Text: "The Moon is on average 384,400 km (238,855 miles) away from Earth."},
	)
	a, err := orchestrix.New(model, orchestrix.WithRubric(cfg), orchestrix.WithMaxTurns(5))
	if err != nil {
		return err
	}

	var final *agent.Final
	for ev, err := range a.Run(ctx, orchestrix.Text("How far is the Moon from Earth on average?")) {
		if err != nil {
			return err
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		return errors.New("run ended without a final event")
	}
	if final.Reason != agent.ReasonCompleted {
		return fmt.Errorf("run stopped: %s", final.Reason)
	}

	// The rubric records every grading of the run in its final State.
	for i, g := range rubric.Grades(final.State) {
		verdict := "fail"
		if g.Pass {
			verdict = "pass"
		}
		fmt.Fprintf(w, "grade %d: %s", i+1, verdict)
		if g.Feedback != "" {
			fmt.Fprintf(w, " (%s)", exmodel.Safe(g.Feedback))
		}
		if g.Error != "" {
			fmt.Fprintf(w, " grader error: %s", exmodel.Safe(g.Error))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "answer: %s\n", exmodel.Safe(final.Text))
	return nil
}
