package skills

import (
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// StandingHeader introduces the instructions of loaded skills that the
// request no longer shows, re-injected at the start of the conversation.
const StandingHeader = "Skill instructions loaded earlier in this conversation (still in effect):"

// standing returns msgs with the instructions of every skill loaded in the
// run (LoadedKey) that scope offers but msgs no longer show, for example
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

// shown reports whether a tool result in msgs still holds the full
// instructions of the skill named name: its text has both Marker(name) and
// the closing tag.
func shown(msgs []llm.Message, name string) bool {
	marker := Marker(name)
	for _, msg := range msgs {
		for _, b := range msg.Content {
			if !isToolResult(b) {
				continue
			}
			text := llm.Message{Content: b.Content}.Text()
			if strings.Contains(text, marker) && strings.Contains(text, skillEnd) {
				return true
			}
		}
	}
	return false
}

func isToolResult(b llm.Block) bool { return b.Type == llm.BlockToolResult }
