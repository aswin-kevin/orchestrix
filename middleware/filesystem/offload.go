package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/internal/pathsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Offload settings.
const (
	// DefaultOffloadChars is the result size, in characters (runes), above
	// which another tool's output is saved to the backend, when
	// Config.OffloadChars is zero.
	DefaultOffloadChars = 80_000
	// MessageOffloadChars is the size, in characters (runes), above which a
	// user text block is saved to the backend and replaced, in the request
	// only, by a preview.
	MessageOffloadChars = 200_000

	toolResultDir  = backend.InternalDir + "/large_tool_results"
	messageDir     = backend.InternalDir + "/large_messages"
	headLines      = 10   // lines of a large result kept inline
	messagePreview = 2000 // characters of a large message kept inline
	hashChars      = 12   // hex characters of the content hash in a message file name
)

// WrapTool implements agent.ToolWrapper. Output of tools this middleware does
// not own, when larger than Config.OffloadChars, is saved to the backend and
// replaced by its first lines and the file path, so one noisy tool cannot
// fill the context. Its own tools are exempt: read_file already pages.
func (m *Middleware) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	res, err := next(ctx, call)
	if err != nil || m.ownsTool(call.Name) {
		return res, err
	}
	text := res.Flatten()
	size := utf8.RuneCountInString(text)
	if size <= m.cfg.OffloadChars {
		return res, nil
	}
	b, err := m.backendFor(call.Call)
	if err != nil {
		return res, nil // nowhere to save it; the loop's own cap still applies
	}
	path := fmt.Sprintf("%s/%s.txt", toolResultDir, pathsafe.Segment(call.ID))
	if err := b.Write(ctx, path, []byte(text)); err != nil {
		return replaceText(res, tool.Truncate(text, m.cfg.OffloadChars)), nil
	}
	notice := fmt.Sprintf("[Output too large (%d chars). Full output saved to %s — use read_file with offset/limit or grep to inspect it.]", size, path)
	return replaceText(res, m.head(text)+"\n…\n"+notice), nil
}

// ownsTool reports whether name is one of the tools this middleware provides.
func (m *Middleware) ownsTool(name string) bool {
	return slices.Contains(allTools, name) && !slices.Contains(m.excluded, name)
}

// head returns the first lines of text, each capped, so that a single huge
// line cannot keep the replacement large.
func (m *Middleware) head(text string) string {
	lines := strings.SplitN(text, "\n", headLines+1)
	if len(lines) > headLines {
		lines = lines[:headLines]
	}
	for i, line := range lines {
		lines[i] = truncateLine(line, m.cfg.MaxLineChars)
	}
	return strings.Join(lines, "\n")
}

// replaceText swaps the text blocks of res for one block, keeping images,
// documents and the error flag.
func replaceText(res tool.Result, text string) tool.Result {
	content := []llm.Block{llm.TextBlock(text)}
	for _, b := range res.Content {
		if b.Type != llm.BlockText {
			content = append(content, b)
		}
	}
	res.Content = content
	return res
}

// offloadLargeMessages returns messages with oversized user text blocks
// replaced by a preview and a pointer to the saved original. It copies what it
// changes: the input, which may be shared with State, is never modified.
func (m *Middleware) offloadLargeMessages(ctx context.Context, b backend.Backend, messages []llm.Message) []llm.Message {
	var out []llm.Message
	for i, msg := range messages {
		if msg.Role != llm.RoleUser {
			continue
		}
		var content []llm.Block
		for j, block := range msg.Content {
			replacement, ok := m.offloadMessageBlock(ctx, b, block)
			if !ok {
				continue
			}
			if content == nil {
				content = slices.Clone(msg.Content)
			}
			content[j] = replacement
		}
		if content == nil {
			continue
		}
		if out == nil {
			out = slices.Clone(messages)
		}
		msg.Content = content
		out[i] = msg
	}
	if out == nil {
		return messages
	}
	return out
}

// offloadMessageBlock saves a large text block and returns its replacement.
// The file name comes from the content hash, so the same text always maps to
// the same file and is written once. ok is false when the block is left as is.
func (m *Middleware) offloadMessageBlock(ctx context.Context, b backend.Backend, block llm.Block) (llm.Block, bool) {
	if block.Type != llm.BlockText || utf8.RuneCountInString(block.Text) <= MessageOffloadChars {
		return block, false
	}
	sum := sha256.Sum256([]byte(block.Text))
	path := fmt.Sprintf("%s/%s.txt", messageDir, hex.EncodeToString(sum[:])[:hashChars])
	// Any Read error is treated as "not saved yet"; a failing Write then ends the offload.
	if _, err := b.Read(ctx, path, 0, 1); err != nil {
		if err := b.Write(ctx, path, []byte(block.Text)); err != nil {
			return block, false
		}
	}
	runes := []rune(block.Text)
	text := fmt.Sprintf("%s\n…\n[Message too large (%d chars). Full text saved to %s — use read_file to inspect it.]",
		string(runes[:messagePreview]), len(runes), path)
	block.Text = text
	return block, true
}
