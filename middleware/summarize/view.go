package summarize

import (
	"encoding/json"
	"slices"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// keptInputChars is how many characters of a shortened tool input remain.
const keptInputChars = 500

// view returns the messages to send: the summary and the messages after the
// boundary when one is set, else current (the loop's view of all of State),
// then micro-compacted. current is a fresh copy owned by the request, so it
// may be edited in place; State is never touched.
func (m *middleware) view(s *state.State, current []llm.Message) []llm.Message {
	msgs := current
	// A boundary beyond the history (State replaced by the host) is ignored
	// rather than trusted.
	if b, ok := Current(s); ok {
		if all := s.Messages(); b.Index <= len(all) {
			msgs = append([]llm.Message{llm.UserText(summaryMessage(b))}, agent.View(all[b.Index:])...)
		}
	}
	m.clearOldResults(msgs)
	m.shortenOldInputs(msgs)
	return msgs
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

// clearOldResults replaces, in place, the content of every tool result other
// than the newest KeepToolResults whose text is longer than ClearResultChars.
func (m *middleware) clearOldResults(msgs []llm.Message) {
	kept := 0
	for i := len(msgs) - 1; i >= 0; i-- {
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
// MaxOldInputChars in assistant messages before the last one with a valid
// JSON object holding the input's beginning. The last assistant message is
// left alone: its calls are the ones being answered now.
func (m *middleware) shortenOldInputs(msgs []llm.Message) {
	last := lastAssistant(msgs)
	for i := range last {
		if msgs[i].Role != llm.RoleAssistant {
			continue
		}
		for j, b := range msgs[i].Content {
			if b.Type == llm.BlockToolUse && utf8.RuneCount(b.Input) > m.cfg.MaxOldInputChars {
				msgs[i].Content[j].Input = shortenedInput(b.Input)
			}
		}
	}
}

// lastAssistant returns the index of the last assistant message, or
// len(msgs) when there is none.
func lastAssistant(msgs []llm.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant {
			return i
		}
	}
	return len(msgs)
}

// shortenedInput returns {"truncated": "<first keptInputChars chars>…"}.
func shortenedInput(input json.RawMessage) json.RawMessage {
	runes := []rune(string(input))
	prefix := string(runes[:min(keptInputChars, len(runes))])
	// A map of strings always encodes.
	out, _ := json.Marshal(map[string]string{"truncated": prefix + "…"})
	return out
}
