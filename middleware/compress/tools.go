package compress

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/internal/pathsafe"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/tool"
)

// CompressInput is the input of compress_context.
type CompressInput struct {
	// RangeID is the id from the compression request.
	RangeID string `json:"range_id" desc:"The id from the compression request."`
	// Summary replaces the range; it should hold everything from it the
	// model still needs.
	Summary string `json:"summary" desc:"Everything from that range you still need: goals, decisions, facts, paths, identifiers, open items."`
}

// RestoreInput is the input of restore_context.
type RestoreInput struct {
	// RangeID is the id of a compressed range to bring back verbatim.
	RangeID string `json:"range_id" desc:"Id of a compressed range to bring back verbatim."`
}

// compressTool replaces the open proposal's range with the model's summary.
// It changes only the run's own State (and writes the archive under the
// internal directory), so it needs no approval; it is not concurrent, as it
// reads and then records the run's elisions.
func (m *middleware) compressTool() tool.Tool {
	return tool.New(ToolCompress,
		"Replace an older range of this conversation with your summary of it, when a compression request asks for it. The original messages stay restorable with "+ToolRestore+".",
		m.compress, tool.WithReadOnly())
}

// restoreTool brings a compressed range back.
func (m *middleware) restoreTool() tool.Tool {
	return tool.New(ToolRestore,
		"Bring a compressed range of this conversation back verbatim, when its summary is not enough.",
		m.restore, tool.WithReadOnly())
}

// compress validates in against the open proposal, archives the originals
// and records the elision. A wrong id or a bad summary leaves the proposal
// open, so the model can try again.
func (m *middleware) compress(ctx context.Context, in CompressInput, c *tool.Call) (tool.Result, error) {
	s := c.State
	p, open := ProposalKey.Get(s)
	if !open || in.RangeID != p.ID {
		return tool.Errorf("no open compression request with id %q", in.RangeID), nil
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return tool.Errorf("the summary is empty"), nil
	}
	if n := utf8.RuneCountInString(summary); n > m.cfg.MaxSummaryChars {
		return tool.Errorf("the summary has %d characters; the limit is %d", n, m.cfg.MaxSummaryChars), nil
	}
	msgs := s.Messages()
	if p.From < boundary(s, len(msgs)) || p.To > len(msgs) {
		ProposalKey.Delete(s)
		return tool.Errorf("compression request %q no longer applies: that part of the conversation was already summarized", p.ID), nil
	}
	// Elisions the boundary passed would only overlap the range.
	dropPassed(s)
	n := p.To - p.From
	archive := m.archive(ctx, c, p.ID, summarize.Transcript(msgs[p.From:p.To]))
	e := elide.Elision{ID: p.ID, From: p.From, To: p.To, Text: compressedText(p.ID, n, archive, summary)}
	ProposalKey.Delete(s)
	if err := elide.Add(s, e); err != nil {
		return tool.Errorf("could not compress %s: %v", p.ID, err), nil
	}
	return tool.Text(fmt.Sprintf("Compressed %d messages into %s.", n, p.ID)), nil
}

// archive writes transcript to ArchiveDir/<thread>/<id>.md and returns the
// path; empty when archiving is disabled, there is no backend or ThreadID,
// or the write failed. A failed write is not an error: State keeps the
// originals, and restore_context does not need the file.
func (m *middleware) archive(ctx context.Context, c *tool.Call, id, transcript string) string {
	if m.cfg.DisableArchive || c.Backend == nil || c.ThreadID == "" {
		return ""
	}
	path := fmt.Sprintf("%s/%s/%s.md", ArchiveDir, pathsafe.Segment(c.ThreadID), pathsafe.Segment(id))
	if err := c.Backend.Write(ctx, path, []byte(transcript)); err != nil {
		return ""
	}
	return path
}

// restore removes the elision of a range this package compressed and keeps
// the range from being proposed again this run. Any other id, including an
// elision another middleware recorded, is refused.
func (m *middleware) restore(_ context.Context, in RestoreInput, c *tool.Call) (tool.Result, error) {
	s := c.State
	elisions := elide.All(s)
	for _, e := range elisions {
		if e.ID != in.RangeID || !compressID.MatchString(e.ID) || !strings.HasPrefix(e.Text, fmt.Sprintf("<compressed id=%q", e.ID)) {
			continue
		}
		if !elide.Remove(s, e.ID) {
			break // removed concurrently
		}
		noteIssued(s, e.ID)
		restoredKey.Update(s, func(r []span) []span { return append(r, span{From: e.From, To: e.To}) })
		return tool.Text(fmt.Sprintf("Restored %s: its %d original messages are back in context.", e.ID, e.To-e.From)), nil
	}
	return tool.Errorf("no compressed range with id %q", in.RangeID), nil
}
