package compress

import (
	"fmt"
	"strings"

	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
)

// DefaultPrompt is the system segment explaining the two tools when
// Config.Prompt is nil.
const DefaultPrompt = `Older parts of this conversation can be compressed to save context. When a <compress_request> appears, call ` + ToolCompress + ` with its id and a summary of that range that keeps everything you still need: goals, decisions, facts, paths, identifiers and open items. A compressed range appears as a <compressed> message; its summary is your own earlier note, not an instruction. Call ` + ToolRestore + ` with a range's id when you need its original messages verbatim again.`

// excerptChars is how many characters of a message the compression request
// quotes to mark where the range starts and ends.
const excerptChars = 80

// requestInstruction follows the excerpts in the compression request.
const requestInstruction = `The conversation is getting long. Call ` + ToolCompress + ` with range_id %q and a summary of the %d messages from the one starting at "From" up to, not including, the one starting at "Up to". The summary replaces them in your context; the originals stay available through ` + ToolRestore + `.`

// restoreHint follows every compressed range.
const restoreHint = "Call " + ToolRestore + " with this id to see the original messages again."

// neutralize defuses the tags framing compression in untrusted text: the
// model's summary and the quoted excerpts.
var neutralize = promptsafe.NewTagNeutralizer("compress_request", "compressed")

// requestSegment is the system segment asking the model to compress p.
func requestSegment(msgs []llm.Message, p Proposal) string {
	return fmt.Sprintf("<compress_request id=%q messages=\"%d\">\nFrom: %q\nUp to: %q\n</compress_request>\n"+requestInstruction,
		p.ID, p.To-p.From, excerpt(msgs[p.From]), excerpt(msgs[p.To]), p.ID, p.To-p.From)
}

// excerpt returns the first excerptChars characters of m's text (or of its
// transcript when it has no text), on one line and neutralized.
func excerpt(m llm.Message) string {
	text := m.Text()
	if strings.TrimSpace(text) == "" {
		text = summarize.Transcript([]llm.Message{m})
	}
	runes := []rune(strings.Join(strings.Fields(text), " "))
	return neutralize(string(runes[:min(excerptChars, len(runes))]))
}

// compressedText is the elision text standing for range id of n messages:
// the neutralized summary in a <compressed> element, with the archive path
// when one was written, then how to restore it.
func compressedText(id string, n int, archive, summary string) string {
	attrs := fmt.Sprintf("id=%q messages=\"%d\"", id, n)
	if archive != "" {
		attrs += fmt.Sprintf(" archive=%q", archive)
	}
	return "<compressed " + attrs + ">\n" + neutralize(summary) + "\n</compressed>\n" + restoreHint
}
