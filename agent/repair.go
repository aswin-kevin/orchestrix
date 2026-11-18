package agent

import (
	"encoding/json"
	"slices"

	"github.com/aswin-kevin/orchestrix/llm"
)

// interruptedToolResult answers a tool call whose result never made it into
// the history, for example because the process stopped mid-call.
const interruptedToolResult = "Tool call was interrupted before it returned."

// emptyToolInput replaces tool_use input that is not valid JSON.
const emptyToolInput = "{}"

// repairHistory returns a copy of msgs that providers will accept: every
// assistant tool_use gets a tool_result in the user message that follows
// (a synthetic interrupted one where it is missing, adding that user message
// if needed), and tool_use input that is not valid JSON becomes {}.
//
// It is applied to the request view only and never modifies msgs, so State
// keeps the conversation exactly as it happened.
func repairHistory(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != llm.RoleAssistant {
			out = append(out, m)
			continue
		}
		m = withValidToolInputs(m)
		out = append(out, m)
		uses := m.ToolUses()
		if len(uses) == 0 {
			continue
		}
		next := llm.Message{Role: llm.RoleUser}
		if i+1 < len(msgs) && msgs[i+1].Role == llm.RoleUser {
			next = msgs[i+1]
			i++ // consumed here
		}
		out = append(out, withResultsFor(uses, next))
	}
	return out
}

// withValidToolInputs returns m with every invalid tool_use input replaced by
// {}. The content is copied on the first change, so m's blocks stay intact.
func withValidToolInputs(m llm.Message) llm.Message {
	var content []llm.Block
	for i, b := range m.Content {
		if b.Type != llm.BlockToolUse || json.Valid(b.Input) {
			continue
		}
		if content == nil {
			content = slices.Clone(m.Content)
		}
		content[i].Input = json.RawMessage(emptyToolInput)
	}
	if content != nil {
		m.Content = content
	}
	return m
}

// withResultsFor returns next with an interrupted tool_result added for each
// call in uses that it does not answer. The added results go after the
// existing ones and before any other content, since providers expect a user
// message to lead with its tool_results.
func withResultsFor(uses []llm.Block, next llm.Message) llm.Message {
	answered := make(map[string]bool)
	for _, b := range next.Content {
		if b.Type == llm.BlockToolResult {
			answered[b.ToolUseID] = true
		}
	}
	var missing []llm.Block
	for _, use := range uses {
		if !answered[use.ID] {
			missing = append(missing, llm.ToolResultText(use.ID, interruptedToolResult, true))
		}
	}
	if len(missing) == 0 {
		return next
	}
	var results, rest []llm.Block
	for _, b := range next.Content {
		if b.Type == llm.BlockToolResult {
			results = append(results, b)
		} else {
			rest = append(rest, b)
		}
	}
	next.Content = slices.Concat(results, missing, rest)
	return next
}
