package orchestrix_test

import (
	"context"
	"fmt"
	"log"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/llm"
)

// ExampleNewDecider asks a decision model a yes/no and a choice question. It
// has no Output comment because it needs a live endpoint.
func ExampleNewDecider() {
	d, err := orchestrix.NewDecider("systemone:your-decision-model")
	if err != nil {
		log.Fatal(err)
	}
	resp, err := d.Decide(context.Background(), llm.DecisionRequest{
		Input: []llm.Block{llm.TextBlock("Please refund my order, it never arrived.")},
		Questions: []llm.Question{
			{Name: "angry", Type: llm.QuestionYesNo, Instructions: "Is the customer angry?"},
			{Name: "topic", Type: llm.QuestionChoice, Instructions: "What is this about?",
				Choices: []llm.Choice{{Value: "refund"}, {Value: "shipping"}, {Value: "other"}}},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.Answers["angry"].Yes, resp.Answers["topic"].Choice)
}
