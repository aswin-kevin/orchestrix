// Command structured-output makes the agent's final answer a Go struct. The
// agent asks the model for a JSON object, checks the reply against the
// struct's schema and asks again when it does not match.
//
//	go run ./examples/structured-output                                 # offline, scripted
//	ORX_MODEL=openai:gpt-4o go run ./examples/structured-output
//
// Offline, the scripted model first answers in prose, then with the JSON
// object, so the run shows one automatic correction.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/examples/internal/exmodel"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func main() {
	err := exmodel.Check() // live: fail early when the provider's API key is missing
	if err == nil {
		err = run(context.Background(), os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "structured-output:", exmodel.Safe(err.Error()))
		os.Exit(1)
	}
}

// Triage is the shape of the answer. The tags drive the generated schema:
// json names the fields, enum limits a string to the listed values and desc
// becomes the field's description for the model.
type Triage struct {
	Severity string   `json:"severity" enum:"low,medium,high" desc:"how urgent the report is"`
	Summary  string   `json:"summary" desc:"one sentence describing the problem"`
	Files    []string `json:"files" desc:"files likely affected"`
}

const report = "Users report that a run sometimes never ends after the turn limit is hit."

func run(ctx context.Context, w io.Writer) error {
	model := exmodel.Model(
		llmtest.Turn{Text: "This looks serious: the loop may not stop at the turn limit."}, // prose: not valid
		llmtest.Turn{Text: `{"severity":"high","summary":"The loop may not stop at the turn limit.","files":["agent/loop.go","agent/budget.go"]}`},
	)
	a, err := orchestrix.New(model,
		orchestrix.WithSystemPrompt("You triage bug reports. Answer from the report alone; you have no files or tools to look at."),
		orchestrix.WithoutMiddleware("filesystem", "planning", "subagents"), // nothing to explore: answer from the report
		orchestrix.WithOutputType[Triage](),
		orchestrix.WithMaxTurns(5),
	)
	if err != nil {
		return err
	}

	t, final, err := orchestrix.RunTyped[Triage](ctx, a, orchestrix.Text(report))
	if err != nil {
		return err
	}

	// Each automatic correction is an injected user message in the state.
	corrections := 0
	for _, m := range final.State.Messages() {
		if m.Meta[agent.InjectedMetaKey] == agent.ContinuationMetaValue {
			corrections++
		}
	}
	fmt.Fprintf(w, "corrections: %d\n", corrections)
	fmt.Fprintf(w, "severity: %s\n", exmodel.Safe(t.Severity))
	fmt.Fprintf(w, "summary: %s\n", exmodel.Safe(t.Summary))
	files := make([]string, len(t.Files))
	for i, f := range t.Files {
		files[i] = exmodel.Safe(f)
	}
	fmt.Fprintf(w, "files: %s\n", strings.Join(files, ", "))
	return nil
}
