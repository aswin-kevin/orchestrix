package orchestrix_test

import (
	"context"
	"fmt"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func Example() {
	// A scripted provider stands in for a real model; in an application pass
	// a spec such as "anthropic:claude-sonnet-4-5" instead.
	model := llmtest.New(llmtest.Turn{Text: "Hello from Orchestrix!"})

	a, err := orchestrix.New(model, orchestrix.WithSystemPrompt("You are friendly."))
	if err != nil {
		panic(err)
	}
	for ev, err := range a.Run(context.Background(), orchestrix.Text("Say hello")) {
		if err != nil {
			panic(err)
		}
		if ev.Kind == agent.KindFinal {
			fmt.Println(ev.Final.Text)
		}
	}
	// Output: Hello from Orchestrix!
}
