package agent

import (
	"context"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/tool"
)

const refusalText = "I can't help with that."

func refusalTurn(calls ...llmtest.Call) llmtest.Turn {
	return llmtest.Turn{Text: refusalText, ToolCalls: calls, StopReason: llm.StopRefusal}
}

func TestRefusalEndsRun(t *testing.T) {
	p := llmtest.New(refusalTurn())
	a := mustNew(t, Config{Provider: p})

	_, final := collect(t, a.Run(t.Context(), userInput("do it")))

	if final.Reason != ReasonRefusal || final.Err != nil || final.Text != refusalText {
		t.Fatalf("final = %+v, want refusal with the reply as text", final)
	}
	msgs := final.State.Messages()
	if last := msgs[len(msgs)-1]; last.Role != llm.RoleAssistant || last.Text() != refusalText {
		t.Fatalf("last state message = %+v, want the refused reply", last)
	}
}

func TestRefusalWithToolCallsDoesNotRunThem(t *testing.T) {
	var ran int
	counting := tool.New("count", "Count calls.",
		func(context.Context, echoInput, *tool.Call) (tool.Result, error) {
			ran++
			return tool.Text("counted"), nil
		})
	p := llmtest.New(
		refusalTurn(llmtest.Call{ID: "c1", Name: "count", Input: map[string]any{"text": "x"}}),
		llmtest.Turn{Text: "ok"},
	)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{counting}})

	_, final := collect(t, a.Run(t.Context(), userInput("do it")))
	if final.Reason != ReasonRefusal || final.Err != nil {
		t.Fatalf("final = %+v, want refusal", final)
	}
	if ran != 0 {
		t.Fatalf("tool ran %d times, want 0", ran)
	}

	_, final = collect(t, a.Run(t.Context(), Input{State: final.State, Messages: []llm.Message{llm.UserText("again")}}))
	if final.Reason != ReasonCompleted || ran != 0 {
		t.Fatalf("second run final = %+v, tool ran %d times; want completed, 0", final, ran)
	}
	var found bool
	for _, m := range p.Requests[1].Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && b.ToolUseID == "c1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("second request = %+v, want a tool_result for c1", p.Requests[1].Messages)
	}
}

func TestRefusalSkipsBeforeStopAndOutputCheck(t *testing.T) {
	var calls int
	p := llmtest.New(refusalTurn(), llmtest.Turn{Text: `{"n": 1}`})
	a := mustNew(t, Config{
		Provider:     p,
		OutputSchema: answerSchema,
		Middleware:   []Middleware{stopHook{prompts: []string{"keep going"}, calls: &calls}},
	})

	_, final := collect(t, a.Run(t.Context(), userInput("do it")))

	if final.Reason != ReasonRefusal || final.Err != nil || final.Output != nil {
		t.Fatalf("final = %+v, want refusal without output", final)
	}
	if calls != 0 || p.Calls() != 1 {
		t.Fatalf("BeforeStop calls = %d, model calls = %d; want 0 and 1", calls, p.Calls())
	}
}

func TestRefusalDuringSettlement(t *testing.T) {
	p := llmtest.New(toolTurn("t1"), refusalTurn(), llmtest.Turn{Text: "spare"})
	a := mustNew(t, Config{
		Provider:   p,
		Tools:      []tool.Tool{echoTool()},
		MaxTurns:   1,
		Settlement: &Settlement{Prompt: settlementPrompt, MaxTurns: 2},
	})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonRefusal || final.Err != nil || final.Text != refusalText {
		t.Fatalf("final = %+v, want refusal", final)
	}
	if p.Calls() != 2 {
		t.Fatalf("model called %d times, want 2", p.Calls())
	}
}
