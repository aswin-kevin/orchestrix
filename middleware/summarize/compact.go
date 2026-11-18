package summarize

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/pathsafe"
	"github.com/aswin-kevin/orchestrix/llm"
)

// minTailMessages is the fewest messages a compaction keeps verbatim.
const minTailMessages = 2

// compact summarizes the messages between the current boundary and the
// recent tail of about keepTokens, and moves the boundary past them. It
// reports whether the boundary moved; false with a nil error means there was
// nothing to summarize. A summarizer error is also recorded in
// Boundary.LastError; success clears it. State messages are never changed.
func (m *middleware) compact(ctx context.Context, req *agent.ModelRequest, keepTokens int) (bool, error) {
	msgs := req.State.Messages()
	prev, _ := Current(req.State)
	if prev.Index > len(msgs) {
		prev = Boundary{} // stale: State was replaced, start over
	}
	k := SafeCut(msgs, prev.Index, len(msgs)-tailCount(msgs, keepTokens))
	if k <= prev.Index {
		return false, nil
	}
	older := msgs[prev.Index:k]
	summary, err := m.summarize(ctx, req, prev.Summary, render(older, DefaultMaxBlockChars))
	if err != nil {
		BoundaryKey.Update(req.State, func(b Boundary) Boundary {
			b.LastError = err.Error()
			return b
		})
		return false, err
	}
	next := Boundary{Index: k, Summary: summary, Compactions: prev.Compactions + 1}
	next.Path = m.offload(ctx, req, next.Compactions, render(older, noBlockCap))
	BoundaryKey.Set(req.State, next)
	return true, nil
}

// tailCount returns the length of the shortest suffix of msgs whose
// estimated size reaches keepTokens, and at least minTailMessages; all of
// msgs when even the whole history is smaller.
func tailCount(msgs []llm.Message, keepTokens int) int {
	chars := 0
	for n := 1; n <= len(msgs); n++ {
		chars += len(renderMessage(msgs[len(msgs)-n], noBlockCap))
		if n >= minTailMessages && chars/CharsPerToken >= keepTokens {
			return n
		}
	}
	return len(msgs)
}

// summarize asks the summarizer for a summary of rendered, the conversation
// after previous.
func (m *middleware) summarize(ctx context.Context, req *agent.ModelRequest, previous, rendered string) (string, error) {
	provider, model := m.cfg.Provider, m.cfg.Model
	if provider == nil {
		provider = req.Provider
	}
	if model == "" {
		model = req.Request.Model
	}
	resp, err := llm.Collect(ctx, provider, llm.Request{
		Model:     model,
		System:    []string{m.prompt},
		Messages:  []llm.Message{llm.UserText("Previous summary:\n" + previous + "\n\nNew conversation:\n" + rendered)},
		MaxTokens: DefaultSummaryMaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("summarize conversation: %w", err)
	}
	summary := strings.TrimSpace(resp.Message.Text())
	if summary == "" {
		return "", errors.New("summarize conversation: summarizer returned no text")
	}
	return summary, nil
}

// offload saves the transcript of compaction number n to its own file and
// returns the path; empty when offload is disabled, there is no backend, the
// run has no ThreadID (threadless runs would overwrite each other's files),
// or the write failed. Each compaction gets a new file, so no file grows past
// what one read can load. A failed write is not an error: State still holds
// every message, the transcript is only a convenience for the model.
func (m *middleware) offload(ctx context.Context, req *agent.ModelRequest, n int, transcript string) string {
	if m.cfg.DisableOffload || req.Backend == nil || req.ThreadID == "" {
		return ""
	}
	path := fmt.Sprintf("%s/%s/%d.md", HistoryDir, pathsafe.Segment(req.ThreadID), n)
	if err := req.Backend.Write(ctx, path, []byte(transcript)); err != nil {
		return ""
	}
	return path
}
