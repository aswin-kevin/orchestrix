package skills

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/state"
)

// StandingHeader introduces the instructions of loaded skills that the
// request no longer shows, re-injected at the start of the conversation.
const StandingHeader = "Skill instructions loaded earlier in this conversation (still in effect):"

// standing returns msgs with the instructions of every skill loaded in the
// run (loadedKey) that scope offers but msgs no longer show, for example
// because compaction summarized or cleared the skill tool's result. They are
// joined under StandingHeader into one text block, appended to the first
// message when it is a plain user message, else sent as a new first user
// message. Pinned skills are skipped: the system prompt carries them already.
//
// msgs is the request's view; the result shares no edited message or
// content slice with it, and State is never touched.
func (m *Middleware) standing(s *state.State, scope Scope, msgs []llm.Message) []llm.Message {
	pinned := slices.Concat(m.pinned, scope.Pinned)
	var blocks []string
	for _, name := range Loaded(s) {
		sk, ok := m.offered(scope, name)
		if !ok || slices.Contains(pinned, name) || shown(msgs, name) {
			continue
		}
		blocks = append(blocks, frame(sk))
	}
	if len(blocks) == 0 {
		return msgs
	}
	text := StandingHeader + "\n\n" + strings.Join(blocks, "\n\n")
	if len(msgs) > 0 && msgs[0].Role == llm.RoleUser && !slices.ContainsFunc(msgs[0].Content, isToolResult) {
		out := slices.Clone(msgs)
		out[0].Content = append(slices.Clone(out[0].Content), llm.TextBlock(text))
		return out
	}
	return slices.Insert(slices.Clone(msgs), 0, llm.UserText(text))
}

// shown reports whether msgs still hold the full instructions of the skill
// named name: the result of a skill tool call (direct or through call_tool)
// that loaded name, not one that read a file of it, whose text has both
// Marker(name) and the closing tag. Results of other tools never count, so
// a file that merely contains the frame cannot stop the re-injection.
func shown(msgs []llm.Message, name string) bool {
	loads := map[string]bool{} // ids of skill tool calls that loaded name
	marker := Marker(name)
	for _, msg := range msgs {
		for _, b := range msg.Content {
			switch {
			case b.Type == llm.BlockToolUse:
				if in, ok := skillInput(b); ok && in.Name == name && in.File == "" {
					loads[b.ID] = true
				}
			case isToolResult(b) && loads[b.ToolUseID]:
				text := llm.Message{Content: b.Content}.Text()
				if strings.Contains(text, marker) && strings.Contains(text, skillEnd) {
					return true
				}
			}
		}
	}
	return false
}

// skillInput returns the input of a tool_use block that calls the skill
// tool, directly or through call_tool.
func skillInput(use llm.Block) (Input, bool) {
	raw := use.Input
	if use.Name == deferred.ToolCall {
		var call deferred.CallInput
		if json.Unmarshal(raw, &call) != nil || call.Name != ToolSkill {
			return Input{}, false
		}
		raw = call.Input
	} else if use.Name != ToolSkill {
		return Input{}, false
	}
	var in Input
	return in, json.Unmarshal(raw, &in) == nil
}

func isToolResult(b llm.Block) bool { return b.Type == llm.BlockToolResult }
