package summarize

import (
	"encoding/json"
	"slices"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// keptInputChars is how many characters of a shortened tool input remain.
const keptInputChars = 500

// view returns the messages to send: the summary and the elided view of the
// messages after the boundary when one is set, else current (the loop's
// elided view of all of State), then micro-compacted. current is a fresh copy
// owned by the request, so it may be edited in place; State is never touched.
func (m *middleware) view(s *state.State, current []llm.Message) []llm.Message {
	msgs := current
	history := s.Messages()
	from := 0
	// A boundary beyond the history (State replaced by the host) is ignored
	// rather than trusted.
	if b, ok := Current(s); ok && b.Index <= len(history) {
		from = b.Index
		msgs = append([]llm.Message{llm.UserText(summaryMessage(b))}, elide.View(s, from)...)
	}
	m.clearOldResults(msgs)
	m.shortenOldInputs(msgs, settledAssistants(history, from, elide.Applied(s, from)))
	return msgs
}

// settledAssistants counts the assistant messages of the view built from
// history[from:] and the applied elisions that come before the last user turn:
// those whose tool inputs may be shortened. Turns are found in history, not
// in the view: the view has lost the Meta that marks injected messages.
// Assistant messages inside an elision are not in the view, so they are not
// counted; the view holds the others in the same order.
func settledAssistants(history []llm.Message, from int, applied []elide.Elision) int {
	end := lastIndex(history[from:], isUserTurn) + from
	n := 0
	for i := from; i < end; i++ {
		if isAssistant(history[i]) && !slices.ContainsFunc(applied, func(e elide.Elision) bool { return e.From <= i && i < e.To }) {
			n++
		}
	}
	return n
}

// summaryMessage is the text of the user message that stands for the
// summarized part of the conversation.
func summaryMessage(b Boundary) string {
	text := "<summary>\n" + b.Summary + "\n</summary>\nEarlier conversation was summarized above."
	if b.Path != "" {
		text += " Its full transcript is in " + b.Path + "."
	}
	return text
}

// SafeCut returns where to split msgs so that msgs[:k] can be summarized and
// msgs[k:] kept: the largest k in (from, want] where msgs[k] is not a user
// message carrying tool_result blocks, so no result is separated from its
// call; from when there is none. k == len(msgs) (nothing kept) counts as
// safe; want is clamped to len(msgs).
func SafeCut(msgs []llm.Message, from, want int) int {
	for k := min(want, len(msgs)); k > from; k-- {
		if k == len(msgs) || !carriesToolResult(msgs[k]) {
			return k
		}
	}
	return from
}

func carriesToolResult(m llm.Message) bool {
	return m.Role == llm.RoleUser && slices.ContainsFunc(m.Content, isToolResult)
}

func isToolResult(b llm.Block) bool { return b.Type == llm.BlockToolResult }

// clearOldResults replaces, in place, the content of every tool result
// before the last assistant message, other than the newest KeepToolResults of
// those, whose text is longer than ClearResultChars. Results after the last
// assistant message answer the current turn and have not been seen by the
// model yet, so they are never cleared.
func (m *middleware) clearOldResults(msgs []llm.Message) {
	kept := 0
	for i := lastIndex(msgs, isAssistant) - 1; i >= 0; i-- {
		content := msgs[i].Content
		for j := len(content) - 1; j >= 0; j-- {
			if !isToolResult(content[j]) {
				continue
			}
			kept++
			if kept > m.cfg.KeepToolResults && utf8.RuneCountInString(resultText(content[j])) > m.cfg.ClearResultChars {
				content[j].Content = []llm.Block{llm.TextBlock(ClearedResultText)}
			}
		}
	}
}

// resultText concatenates the text blocks of a tool result.
func resultText(b llm.Block) string {
	return llm.Message{Content: b.Content}.Text()
}

// shortenOldInputs replaces, in place, every tool_use input longer than
// MaxOldInputChars in the first settled assistant messages of msgs with a
// valid JSON object holding the input's beginning. settled counts the
// assistant messages before the last user turn; the tool loop after it is
// left alone: it is the turn in progress, and providers may validate its
// signed thinking against the inputs as they were sent.
func (m *middleware) shortenOldInputs(msgs []llm.Message, settled int) {
	for i := range msgs {
		if settled == 0 {
			return
		}
		if msgs[i].Role != llm.RoleAssistant {
			continue
		}
		settled--
		for j, b := range msgs[i].Content {
			if b.Type == llm.BlockToolUse && utf8.RuneCount(b.Input) > m.cfg.MaxOldInputChars {
				msgs[i].Content[j].Input = shortenedInput(b.Input)
			}
		}
	}
}

// lastIndex returns the index of the last message matching f, or -1.
func lastIndex(msgs []llm.Message, f func(llm.Message) bool) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if f(msgs[i]) {
			return i
		}
	}
	return -1
}

func isAssistant(m llm.Message) bool { return m.Role == llm.RoleAssistant }

// isUserTurn reports a user message that starts a turn: neither a set of
// tool results nor a message a middleware injected (agent.InjectedMetaKey).
func isUserTurn(m llm.Message) bool {
	return m.Role == llm.RoleUser && !carriesToolResult(m) && m.Meta[agent.InjectedMetaKey] == ""
}

// shortenedInput returns {"truncated": "<first keptInputChars chars>…"}.
func shortenedInput(input json.RawMessage) json.RawMessage {
	runes := []rune(string(input))
	prefix := string(runes[:min(keptInputChars, len(runes))])
	// A map of strings always encodes.
	out, _ := json.Marshal(map[string]string{"truncated": prefix + "…"})
	return out
}
