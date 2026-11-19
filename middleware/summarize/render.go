package summarize

import (
	"strings"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// DefaultMaxBlockChars caps each content block in the transcript sent to the
// summarizer, so one huge tool result cannot crowd out the rest.
const DefaultMaxBlockChars = 2000

// noBlockCap renders blocks whole, for the offloaded transcript and for size
// estimates.
const noBlockCap = 0

// render writes msgs as plain text: a "## user" or "## assistant" heading per
// message, then one line per block. Blocks longer than maxBlockChars are
// truncated; noBlockCap keeps them whole. Thinking is left out: it is the
// model's scratch work, not part of the conversation.
func render(msgs []llm.Message, maxBlockChars int) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, renderMessage(m, maxBlockChars))
	}
	return strings.Join(parts, "\n\n")
}

func renderMessage(m llm.Message, maxBlockChars int) string {
	lines := []string{"## " + string(m.Role)}
	for _, b := range m.Content {
		if line, ok := renderBlock(b); ok {
			lines = append(lines, tool.Truncate(line, maxBlockChars))
		}
	}
	return strings.Join(lines, "\n")
}

// renderBlock returns the transcript line for b; ok is false for blocks the
// transcript leaves out.
func renderBlock(b llm.Block) (line string, ok bool) {
	switch b.Type {
	case llm.BlockText:
		return b.Text, true
	case llm.BlockToolUse:
		return "tool_call " + b.Name + " " + string(b.Input), true
	case llm.BlockToolResult:
		return "tool_result " + b.ToolUseID + ": " + renderResult(b), true
	case llm.BlockImage, llm.BlockDocument:
		return "[" + string(b.Type) + "]", true
	}
	return "", false
}

// renderResult flattens a tool result's content: its text, with a
// placeholder for each image or document.
func renderResult(b llm.Block) string {
	var parts []string
	for _, c := range b.Content {
		if line, ok := renderBlock(c); ok {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "\n")
}

// Transcript renders msgs as the plain-text transcript summarize offloads: a
// "## user" or "## assistant" heading per message, then one line per text,
// tool call, tool result or attachment, none truncated. Thinking is left out.
func Transcript(msgs []llm.Message) string {
	return render(msgs, noBlockCap)
}
