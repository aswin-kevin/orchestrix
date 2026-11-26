package agent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func toolUse(id, input string) llm.Block {
	return llm.Block{Type: llm.BlockToolUse, ID: id, Name: "t", Input: json.RawMessage(input)}
}

func assistant(blocks ...llm.Block) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: blocks}
}

func user(blocks ...llm.Block) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: blocks}
}

func interrupted(id string) llm.Block {
	return llm.ToolResultText(id, interruptedToolResult, true)
}

func TestRepairHistory(t *testing.T) {
	ok := llm.ToolResultText("a", "fine", false)
	tests := []struct {
		name string
		in   []llm.Message
		want []llm.Message
	}{
		{
			name: "paired history is unchanged",
			in:   []llm.Message{llm.UserText("hi"), assistant(toolUse("a", `{}`)), user(ok)},
			want: []llm.Message{llm.UserText("hi"), assistant(toolUse("a", `{}`)), user(ok)},
		},
		{
			name: "dangling call at the end gets a new user message",
			in:   []llm.Message{llm.UserText("hi"), assistant(toolUse("a", `{}`))},
			want: []llm.Message{llm.UserText("hi"), assistant(toolUse("a", `{}`)), user(interrupted("a"))},
		},
		{
			name: "missing result is added to the existing next user message",
			in:   []llm.Message{assistant(toolUse("a", `{}`), toolUse("b", `{}`)), user(ok)},
			want: []llm.Message{assistant(toolUse("a", `{}`), toolUse("b", `{}`)), user(ok, interrupted("b"))},
		},
		{
			name: "dangling call followed by an assistant message",
			in:   []llm.Message{assistant(toolUse("a", `{}`)), assistant(llm.TextBlock("later"))},
			want: []llm.Message{assistant(toolUse("a", `{}`)), user(interrupted("a")), assistant(llm.TextBlock("later"))},
		},
		{
			name: "invalid input is replaced with an empty object",
			in:   []llm.Message{assistant(toolUse("a", `{"x":`)), user(ok)},
			want: []llm.Message{assistant(toolUse("a", `{}`)), user(ok)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repairHistory(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("repairHistory =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestRepairHistoryDoesNotMutateInput(t *testing.T) {
	in := []llm.Message{assistant(toolUse("a", `{"x":`), toolUse("b", `{}`)), user(llm.ToolResultText("a", "ok", false))}
	before := []llm.Message{assistant(toolUse("a", `{"x":`), toolUse("b", `{}`)), user(llm.ToolResultText("a", "ok", false))}

	repairHistory(in)

	if !reflect.DeepEqual(in, before) {
		t.Fatalf("input changed:\n%+v", in)
	}
}

func TestRunRepairsRequestViewOnly(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a := mustNew(t, Config{Provider: p})
	history := []llm.Message{llm.UserText("hi"), assistant(toolUse("a", `{"x":`)), llm.UserText("again")}

	_, final := collect(t, a.Run(t.Context(), Input{Messages: history}))

	view := p.Requests[0].Messages
	if string(view[1].Content[0].Input) != `{}` || view[2].Content[0].Type != llm.BlockToolResult {
		t.Fatalf("request view not repaired: %+v", view)
	}
	stored := final.State.Messages()
	if string(stored[1].Content[0].Input) != `{"x":` || len(stored[2].Content) != 1 {
		t.Fatalf("state was modified by repair: %+v", stored)
	}
}
