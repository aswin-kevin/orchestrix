package planning_test

import (
	"context"
	"fmt"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
)

// ExampleNew shows a scripted model writing a todo list read back with Todos.
func ExampleNew() {
	mw, err := planning.New(planning.Config{})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	model := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{
			ID:   "t1",
			Name: planning.ToolWriteTodos,
			Input: map[string]any{"todos": []map[string]any{
				{"content": "Write the code", "status": "completed"},
				{"content": "Test the code", "status": "in_progress"},
			}},
		}}},
		llmtest.Turn{Text: "Plan recorded."},
	)
	a, err := agent.New(agent.Config{Provider: model, Middleware: []agent.Middleware{mw}})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	var final *agent.Final
	in := agent.Input{Messages: []llm.Message{llm.UserText("Plan the work.")}}
	for ev, err := range a.Run(context.Background(), in) {
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		fmt.Println("error: no final event")
		return
	}
	fmt.Println(final.Text)
	for _, t := range planning.Todos(final.State) {
		fmt.Printf("%s: %s\n", t.Status, t.Content)
	}
	// Output:
	// Plan recorded.
	// completed: Write the code
	// in_progress: Test the code
}
