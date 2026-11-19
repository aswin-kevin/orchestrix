// Command background-tasks lets the agent hand work to a subagent that runs
// in the background and finish its turn without waiting. When the task
// ends, the thread's next model call is told with a task notification, and
// the agent answers from it.
//
//	go run ./examples/background-tasks                           # offline, scripted
//	ORX_MODEL=openai:gpt-4o go run ./examples/background-tasks
//
// The model gets start_task, check_task, update_task, cancel_task and
// list_tasks. Here it starts a researcher, checks on it once, and ends the
// first prompt while the researcher works; the second prompt on the same
// thread receives the notification with the researcher's result.
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
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
)

func main() {
	ctx := context.Background()
	err := exmodel.Check() // live: fail early when the provider's API key is missing
	if err == nil {
		err = run(ctx, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "background-tasks:", exmodel.Safe(err.Error()))
		os.Exit(1)
	}
}

// script is the main model's offline script, one turn per model call.
var script = []llmtest.Turn{
	// First prompt: start the researcher...
	{ToolCalls: []llmtest.Call{{ID: "call_1", Name: async.ToolStart, Input: map[string]string{
		"subagent_type": "researcher",
		"description":   "Find one surprising fact about honey.",
	}}}},
	// ...look in on it without waiting (task ids count up from async_1)...
	{ToolCalls: []llmtest.Call{{ID: "call_2", Name: async.ToolCheck, Input: map[string]string{"task_id": "async_1"}}}},
	// ...and end the turn while it works.
	{Text: "The researcher is on it; I will report back when it finishes."},
	// Second prompt: the notification is in this request; answer from it.
	{Text: "The researcher reports that archaeologists have found edible honey in ancient Egyptian tombs: honey never spoils."},
}

func run(ctx context.Context, w io.Writer) error {
	return runModel(ctx, w, exmodel.Model(script...))
}

// runModel runs the example on model, a spec string or an llm.Provider.
func runModel(ctx context.Context, w io.Writer, model any) error {
	// Offline, the researcher's script waits for release, so it is still
	// running when the first prompt ends, as real research would be. Live,
	// the researcher uses the main model.
	release := make(chan struct{})
	researcher := subagents.Subagent{
		Name:         "researcher",
		Description:  "Looks up a fact and reports it in one or two sentences.",
		SystemPrompt: "You research the question you are given and answer in at most two sentences.",
		Provider: exmodel.SubHeld(release, llmtest.Turn{
			Text: "Honey never spoils: archaeologists have found edible honey in ancient Egyptian tombs.",
		}),
	}
	tasks, err := async.New(async.Config{
		Subagents: []subagents.Subagent{researcher},
		// By default a run that ends while its tasks still run is reminded
		// of them once. This host ends the first prompt on purpose and
		// prompts again later, so the reminder is off.
		DisableStopReminder: true,
	})
	if err != nil {
		return err
	}
	defer tasks.Shutdown() // stops background subagents still running

	a, err := orchestrix.New(model,
		orchestrix.WithSystemPrompt("Delegate research to the researcher in the background. Do not wait for it: end your turn, and report its finding once you are told it finished."),
		orchestrix.WithAsync(tasks),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		return err
	}

	// Background tasks belong to a thread: the one that started them is told
	// when they end.
	th, err := a.Thread(ctx, "honey")
	if err != nil {
		return err
	}
	if err := prompt(ctx, w, th, "Find me one surprising fact about honey."); err != nil {
		return err
	}

	close(release) // offline: the research can now finish
	// A real host would carry on and prompt whenever it likes; waiting here
	// makes sure the task has ended, so the output is the same every run.
	if err := exmodel.WaitTasks(ctx, a, th); err != nil {
		return err
	}
	if err := prompt(ctx, w, th, "Any news from the researcher?"); err != nil {
		return err
	}

	// The notification is a user message the async middleware added to the
	// thread right before a model call; show its first line.
	for _, m := range th.Messages() {
		if m.Meta[async.NotificationMetaKey] != "" {
			headline, _, _ := strings.Cut(m.Text(), "\n")
			fmt.Fprintf(w, "notice: %s\n", exmodel.Safe(headline))
		}
	}
	return nil
}

// prompt runs text on th and prints the tool activity and the answer.
func prompt(ctx context.Context, w io.Writer, th *agent.Thread, text string) error {
	fmt.Fprintf(w, "you: %s\n", text)
	for ev, err := range th.Prompt(ctx, text) {
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
