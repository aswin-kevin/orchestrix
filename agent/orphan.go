package agent

import (
	"slices"

	"github.com/aswin-kevin/orchestrix/llm"
)

// orphanSite is a tool_use that State holds without a tool_result: assistant
// is the index of its assistant message and use the unanswered block.
type orphanSite struct {
	assistant int
	use       llm.Block
}

// findOrphans returns the unanswered tool_use blocks of msgs in history
// order. A call is answered only by a tool_result carrying its id in the user
// message directly after its assistant message, the rule repairHistory uses.
func findOrphans(msgs []llm.Message) []orphanSite {
	var sites []orphanSite
	for i, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		answered := make(map[string]bool)
		if i+1 < len(msgs) && msgs[i+1].Role == llm.RoleUser {
			for _, b := range msgs[i+1].Content {
				if b.Type == llm.BlockToolResult {
					answered[b.ToolUseID] = true
				}
			}
		}
		for _, use := range m.ToolUses() {
			if !answered[use.ID] {
				sites = append(sites, orphanSite{assistant: i, use: use})
			}
		}
	}
	return sites
}

// placeResults writes the answers into State. answers is keyed by assistant
// message index, each value holding tool_result blocks in tool_use order.
// Where the message after the assistant one is a user message, the blocks go
// into it after its existing tool_results and before its other content;
// otherwise they become a new user message inserted right after the assistant
// one. Indices are processed in descending order so an insertion never moves
// one still to do. Messages are replaced, never mutated in place.
func (r *run) placeResults(answers map[int][]llm.Block) {
	indices := make([]int, 0, len(answers))
	for i := range answers {
		indices = append(indices, i)
	}
	slices.SortFunc(indices, func(a, b int) int { return b - a })
	for _, i := range indices {
		blocks := answers[i]
		history := r.state.Messages()
		if i+1 < len(history) && history[i+1].Role == llm.RoleUser {
			updated := slices.Clone(history)
			updated[i+1] = withBlocksAfterResults(history[i+1], blocks)
			r.state.SetMessages(updated)
			continue
		}
		r.insertAfter(i, []llm.Message{{Role: llm.RoleUser, Content: slices.Clone(blocks)}})
	}
}
