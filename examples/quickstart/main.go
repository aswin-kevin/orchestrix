// Command quickstart builds an agent with one typed tool and asks it a
// question it answers by calling that tool.
//
//	go run ./examples/quickstart                                  # offline, scripted
//	ORX_MODEL=anthropic:claude-sonnet-4-5 go run ./examples/quickstart
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/examples/internal/exmodel"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/tool"
)

func main() {
	err := exmodel.Check() // live: fail early when the provider's API key is missing
	if err == nil {
		err = run(context.Background(), os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "quickstart:", exmodel.Safe(err.Error()))
		os.Exit(1)
	}
}

// addInput is the add tool's input. The JSON schema the model sees is
// derived from the struct: field names from the json tags, descriptions from
// the desc tags.
type addInput struct {
	A int `json:"a" desc:"The first number."`
	B int `json:"b" desc:"The second number."`
}

// add is a typed tool: orchestrix decodes and validates the model's input
// into addInput before the function runs. It only computes, so it is marked
// read-only and needs no permission.
var add = tool.New("add", "Adds two integers and returns the sum.",
	func(_ context.Context, in addInput, _ *tool.Call) (tool.Result, error) {
		return tool.Text(strconv.Itoa(in.A + in.B)), nil
	},
	tool.WithReadOnly(),
)

func run(ctx context.Context, w io.Writer) error {
	// Offline, the model is a script: first it calls add, then it answers.
	model := exmodel.Model(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "call_1", Name: "add", Input: addInput{A: 2, B: 3}}}},
		llmtest.Turn{Text: "2 + 3 = 5."},
	)
	a, err := exmodel.New(model,
		orchestrix.WithSystemPrompt("You are a calculator. Use the add tool for every sum."),
		orchestrix.WithTools(add),
		orchestrix.WithMaxTurns(5),
	)
	if err != nil {
		return err
	}

	// Run streams events; the last one is always KindFinal.
	for ev, err := range a.Run(ctx, orchestrix.Text("What is 2 + 3?")) {
		if err != nil {
			return err // only the final event carries an error
		}
		switch ev.Kind {
		case agent.KindToolUse:
			fmt.Fprintf(w, "tool call: %s %s\n", exmodel.Safe(ev.ToolName), exmodel.Safe(string(ev.Input)))
		case agent.KindToolResult:
			if ev.ToolResult == nil {
				continue
			}
			fmt.Fprintf(w, "tool result: %s\n", exmodel.Safe(ev.ToolResult.Flatten()))
		case agent.KindFinal:
			if ev.Final.Reason != agent.ReasonCompleted {
				return fmt.Errorf("run stopped: %s", ev.Final.Reason)
			}
			fmt.Fprintf(w, "answer: %s\n", exmodel.Safe(ev.Final.Text))
		}
	}
	return nil
}
