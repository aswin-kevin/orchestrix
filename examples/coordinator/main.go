// Command coordinator shows the two ways to run work in parallel: a
// coordinator agent that delegates to worker subagents and combines their
// results, and RunParallel, which runs many independent prompts at once.
//
//	go run ./examples/coordinator                                # offline, scripted
//	ORX_MODEL=anthropic:claude-sonnet-4-5 go run ./examples/coordinator
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/coordinator"
	"github.com/aswin-kevin/orchestrix/examples/internal/exmodel"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
)

func main() {
	err := exmodel.Check() // live: fail early when the provider's API key is missing
	if err == nil {
		err = run(context.Background(), os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "coordinator:", exmodel.Safe(err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, w io.Writer) error {
	if err := delegate(ctx, w); err != nil {
		return err
	}
	return parallel(ctx, w)
}

// delegate runs a coordinator that starts a researcher and a writer in the
// background, waits for both and combines what they return.
func delegate(ctx context.Context, w io.Writer) error {
	// Offline every agent replays its own script; live, the workers use the
	// coordinator's model.
	workers := []subagents.Subagent{
		{
			Name:         "researcher",
			Description:  "Finds facts on a topic.",
			SystemPrompt: "You research a topic and report three short facts.",
			Provider:     exmodel.Sub(llmtest.Turn{Text: "Lithium-ion home batteries store 5 to 15 kWh and last about ten years."}),
		},
		{
			Name:         "writer",
			Description:  "Writes short, clear copy.",
			SystemPrompt: "You write one punchy sentence on the topic you are given.",
			Provider:     exmodel.Sub(llmtest.Turn{Text: "Keep the lights on when the grid goes dark."}),
		},
	}
	model := exmodel.Model(
		// The coordinator starts both workers at once...
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "call_1", Name: async.ToolStart, Input: map[string]string{"subagent_type": "researcher", "description": "Research home battery storage."}},
			{ID: "call_2", Name: async.ToolStart, Input: map[string]string{"subagent_type": "writer", "description": "Write a tagline for home battery storage."}},
		}},
		// ...waits for both...
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "call_3", Name: async.ToolCheck, Input: map[string]any{"task_id": "async_1", "wait": true}},
			{ID: "call_4", Name: async.ToolCheck, Input: map[string]any{"task_id": "async_2", "wait": true}},
		}},
		// ...and combines their results.
		llmtest.Turn{Text: "Keep the lights on when the grid goes dark: a home battery stores 5 to 15 kWh and lasts about ten years."},
	)
	c, err := coordinator.New(model, coordinator.Config{Workers: workers, MaxConcurrency: 2},
		orchestrix.WithMaxTurns(20))
	if err != nil {
		return err
	}
	defer c.Shutdown() // stops background workers still running

	// Background workers belong to a thread, so the run gets one.
	in := orchestrix.Text("Write a two-line pitch for home battery storage: one fact, one tagline.")
	in.ThreadID = "pitch"
	for ev, err := range c.Run(ctx, in) {
		if err != nil {
			return err
		}
		switch ev.Kind {
		case agent.KindToolUse:
			if ev.ParentToolID == "" { // the coordinator's own calls, not its workers'
				fmt.Fprintf(w, "tool call: %s %s\n", exmodel.Safe(ev.ToolName), exmodel.Safe(string(ev.Input)))
			}
		case agent.KindToolResult:
			if ev.ParentToolID == "" && ev.ToolName == async.ToolCheck && ev.ToolResult != nil {
				fmt.Fprintf(w, "worker result: %s\n", exmodel.Safe(ev.ToolResult.Flatten()))
			}
		case agent.KindFinal:
			if ev.Final.Reason != agent.ReasonCompleted {
				return fmt.Errorf("coordinator stopped: %s", ev.Final.Reason)
			}
			fmt.Fprintf(w, "coordinator: %s\n\n", exmodel.Safe(ev.Final.Text))
		}
	}
	return nil
}

// questions are the prompts parallel runs side by side, each with the
// answer its offline script gives.
var questions = []struct{ question, offline string }{
	{"What is the boiling point of water at sea level?", "100 °C (212 °F)."},
	{"How many continents are there?", "Seven."},
	{"Which planet is closest to the Sun?", "Mercury."},
}

// parallel answers every question at once with RunParallel and prints the
// results in question order.
func parallel(ctx context.Context, w io.Writer) error {
	// Live, one plain agent answers every question: jobs may share an agent
	// when each has its own ThreadID, so their threads and workspaces stay
	// apart. Offline, each
	// job gets an agent with its own script, so every answer is paired with
	// its question whichever job reaches the model first.
	var shared *agent.Agent
	if exmodel.Live() {
		a, err := orchestrix.New(exmodel.Model(), orchestrix.WithMaxTurns(5))
		if err != nil {
			return err
		}
		shared = a
	}
	jobs := make([]coordinator.Job, len(questions))
	for i, q := range questions {
		a := shared
		if a == nil {
			var err error
			if a, err = orchestrix.New(exmodel.Model(llmtest.Turn{Text: q.offline}), orchestrix.WithMaxTurns(5)); err != nil {
				return err
			}
		}
		in := orchestrix.Text(q.question)
		in.ThreadID = fmt.Sprintf("question-%d", i+1)
		jobs[i] = coordinator.Job{Agent: a, Input: in}
	}

	results := coordinator.RunParallel(ctx, jobs, coordinator.ParallelOptions{MaxConcurrency: 3})
	for i, r := range results { // one Result per job, in job order
		fmt.Fprintf(w, "%d. %s\n", i+1, questions[i].question)
		switch {
		case r.Err != nil:
			fmt.Fprintf(w, "   failed: %s\n", exmodel.Safe(r.Err.Error()))
		case r.Final.Reason != agent.ReasonCompleted:
			fmt.Fprintf(w, "   stopped: %s\n", r.Final.Reason)
		default:
			fmt.Fprintf(w, "   %s\n", exmodel.Safe(r.Final.Text))
		}
	}
	return nil
}
