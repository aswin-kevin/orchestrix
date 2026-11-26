package summarize

import (
	"context"

	"github.com/aswin-kevin/orchestrix/tool"
)

// CompactInput is the input of compact_conversation, which takes none.
type CompactInput struct{}

func compactTool() tool.Tool {
	return tool.New(ToolCompact,
		"Summarize the older part of this conversation to free up context. Use it when the conversation has grown long and its early details are no longer needed verbatim, for example between two tasks. Recent messages are kept as they are, and the full transcript stays readable in a file.",
		requestCompaction,
		// The tool only flags the run's own private state; it never touches
		// the workspace or the outside world, so it runs without approval
		// in every mode and alongside other calls.
		tool.WithReadOnly(),
		tool.WithConcurrent(),
	)
}

// requestCompaction flags the run: the next model call compacts first.
func requestCompaction(_ context.Context, _ CompactInput, c *tool.Call) (tool.Result, error) {
	requestedKey.Set(c.State, true)
	return tool.Text("The conversation will be compacted before your next turn."), nil
}
