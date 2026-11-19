// Command background-tasks lets the agent hand work to a subagent that runs
// in the background, then wait for its result and answer with it.
//
//	go run ./examples/background-tasks                           # offline, scripted
//	ORX_MODEL=openai:gpt-4o go run ./examples/background-tasks
//
// The model gets start_task, check_task, update_task, cancel_task and
// list_tasks. It could keep working while the task runs; this example keeps
// it short and waits with check_task.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/examples/internal/exmodel"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "background-tasks:", exmodel.Safe(err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, w io.Writer) error {
	// The researcher is a subagent the model can start in the background.
	// Offline it has its own script; live it uses the main model.
	researcher := subagents.Subagent{
		Name:         "researcher",
		Description:  "Looks up a fact and reports it in one or two sentences.",
		SystemPrompt: "You research the question you are given and answer in at most two sentences.",
		Provider: exmodel.Sub(llmtest.Turn{
			Text: "Honey never spoils: archaeologists have found edible honey in ancient Egyptian tombs.",
		}),
	}
	tasks, err := async.New(async.Config{Subagents: []subagents.Subagent{researcher}})
	if err != nil {
		return err
	}
	defer tasks.Shutdown() // stops background subagents still running

	model := exmodel.Model(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "call_1", Name: async.ToolStart, Input: map[string]string{
			"subagent_type": "researcher",
			"description":   "Find one surprising fact about honey.",
		}}}},
		// Task ids are numbered in start order: the first one is async_1.
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "call_2", Name: async.ToolCheck, Input: map[string]any{
			"task_id": "async_1",
			"wait":    true,
		}}}},
		llmtest.Turn{Text: "Here is what the researcher found: honey never spoils."},
	)
	a, err := orchestrix.New(model,
		orchestrix.WithSystemPrompt("Delegate research to the researcher in the background, then report its finding."),
		orchestrix.WithAsync(tasks),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		return err
	}

	// Background tasks belong to a thread: the one that started them is the
	// one told about their progress.
	th, err := a.Thread(ctx, "honey")
	if err != nil {
		return err
	}
	for ev, err := range th.Prompt(ctx, "Find me one surprising fact about honey.") {
		if err != nil {
			return err
		}
		switch ev.Kind {
		case agent.KindToolUse:
			fmt.Fprintf(w, "tool call: %s %s\n", exmodel.Safe(ev.ToolName), exmodel.Safe(string(ev.Input)))
		case agent.KindToolResult:
			if ev.ToolResult != nil {
				fmt.Fprintf(w, "tool result: %s\n", exmodel.Safe(ev.ToolResult.Flatten()))
			}
		case agent.KindFinal:
			if ev.Final.Reason != agent.ReasonCompleted {
				return fmt.Errorf("run stopped: %s", ev.Final.Reason)
			}
			fmt.Fprintf(w, "answer: %s\n", exmodel.Safe(ev.Final.Text))
		}
	}
	return nil
}
