package agent

import (
	"bytes"
	"maps"

	"github.com/aswin-kevin/orchestrix/llm"
)

// View returns msgs as the loop sends them to the model: a deep copy with
// every message's host-only Meta dropped and the history repaired (each
// tool_use answered, invalid tool input replaced by {}; see repairHistory).
// Middleware that makes its own model calls over the conversation, such as
// a summarizer, uses it to see what the model sees. msgs is never modified,
// and editing the result never touches msgs.
func View(msgs []llm.Message) []llm.Message {
	return repairHistory(requestView(msgs))
}

// requestView copies msgs for a model request, dropping each message's
// host-only Meta. The content is copied all the way down (nested tool_result
// content, tool inputs, sources, provider data), because wrappers may edit
// the view in place and State must not see it.
func requestView(msgs []llm.Message) []llm.Message {
	view := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		m.Content = cloneBlocks(m.Content)
		m.Meta = nil
		view[i] = m
	}
	return view
}

// cloneBlocks returns a deep copy of blocks.
func cloneBlocks(blocks []llm.Block) []llm.Block {
	if blocks == nil {
		return nil
	}
	out := make([]llm.Block, len(blocks))
	for i, b := range blocks {
		b.Input = bytes.Clone(b.Input)
		b.Content = cloneBlocks(b.Content)
		b.Source = clonePtr(b.Source)
		b.ProviderData = maps.Clone(b.ProviderData)
		out[i] = b
	}
	return out
}
