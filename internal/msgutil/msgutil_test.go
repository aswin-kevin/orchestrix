package msgutil_test

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/msgutil"
	"github.com/aswin-kevin/orchestrix/llm"
)

func user(blocks ...llm.Block) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: blocks}
}

func text(s string) llm.Block { return llm.Block{Type: llm.BlockText, Text: s} }

func result(id string) llm.Block {
	return llm.Block{Type: llm.BlockToolResult, ToolUseID: id}
}

func injected() llm.Message {
	m := user(text("continue"))
	m.Meta = map[string]string{agent.InjectedMetaKey: agent.ContinuationMetaValue}
	return m
}

func TestCarriesToolResult(t *testing.T) {
	tests := []struct {
		name string
		msg  llm.Message
		want bool
	}{
		{"plain user text", user(text("hi")), false},
		{"tool result only", user(result("a")), true},
		{"text and tool result", user(text("hi"), result("a")), true},
		{"empty content", user(), false},
		{"assistant with tool result block", llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{result("a")}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := msgutil.CarriesToolResult(tt.msg); got != tt.want {
				t.Errorf("CarriesToolResult = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsUserTurn(t *testing.T) {
	tests := []struct {
		name string
		msg  llm.Message
		want bool
	}{
		{"plain user text", user(text("hi")), true},
		{"empty content", user(), true},
		{"tool result only", user(result("a")), false},
		{"text and tool result", user(text("hi"), result("a")), false},
		{"injected user message", injected(), false},
		{"assistant text", llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{text("hi")}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := msgutil.IsUserTurn(tt.msg); got != tt.want {
				t.Errorf("IsUserTurn = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserText(t *testing.T) {
	asst := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{text("no")}}
	tests := []struct {
		name string
		msgs []llm.Message
		want string
	}{
		{"none", nil, ""},
		{"one", []llm.Message{user(text("a"))}, "a"},
		{"split over blocks", []llm.Message{user(text("a"), text("b"))}, "ab"},
		{"skips assistant and empty", []llm.Message{user(text("a")), asst, user(), user(text("b"))}, "a\nb"},
		{"tool result only is empty", []llm.Message{user(result("x"))}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := msgutil.UserText(tt.msgs); got != tt.want {
				t.Errorf("UserText = %q, want %q", got, tt.want)
			}
		})
	}
}
