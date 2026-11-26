package summarize

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/internal/pathsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// minTailMessages is the fewest messages a compaction keeps verbatim.
const minTailMessages = 2

// maxSummaryInputChars bounds the rendered conversation one summarizer
// request carries; the newest part of a longer one is sent (see
// boundedRender).
const maxSummaryInputChars = 400_000

// observed is what the last model response reported about the request it
// answered.
type observed struct {
	// Input is the input-side tokens of that request: Input, CacheRead and
	// CacheWrite of the response's usage.
	Input int `json:"input"`
	// Messages is how many State messages the request was built from.
	Messages int `json:"messages"`
	// Elisions identifies the elisions recorded then (elide.All; see
	// elisionsMark); empty when there were none. Input no longer describes
	// the view once they change.
	Elisions string `json:"elisions,omitempty"`
}

// observedKey holds the last observed; absent until a response reported usage,
// and again after a compaction, which makes the observation stale.
var observedKey = state.NewKey[observed]("summarize.observed", state.Private[observed]())

// failuresKey counts compaction failures in a row. At MaxFailures automatic
// compaction stops until a compaction succeeds or a new run starts.
var failuresKey = state.NewKey[int]("summarize.failures", state.Private[int]())

// Estimate returns the estimated size, in tokens, of the next request: the
// input tokens the last response reported plus the messages added since; a
// pure character estimate of view when nothing was observed, or when the
// elisions (elide.All) changed since the observation. view should be
// the messages as they would be sent, after any compaction. The fallback
// leaves out the system prompt and tool definitions, so it underestimates;
// the reactive path catches what it misses.
func Estimate(s *state.State, view []llm.Message) int {
	msgs := s.Messages()
	if o, ok := observedKey.Get(s); ok && o.Messages <= len(msgs) && o.Elisions == elisionsMark(s) {
		return o.Input + charCount(msgs[o.Messages:])/CharsPerToken
	}
	return charCount(view) / CharsPerToken
}

// elisionsMark returns the IDs of the elisions recorded in s, quoted and
// comma-separated in stored order, so that any change to them changes the mark;
// empty when there are none.
func elisionsMark(s *state.State) string {
	elisions := elide.All(s)
	ids := make([]string, len(elisions))
	for i, e := range elisions {
		ids[i] = strconv.Quote(e.ID)
	}
	return strings.Join(ids, ",")
}

// charCount is the size of msgs in characters, as rendered for a summary.
func charCount(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(renderMessage(m, noBlockCap))
	}
	return n
}

// compact summarizes the messages between the current boundary and the
// recent tail of about keepTokens, and moves the boundary past them. It
// reports whether the boundary moved; false with a nil error means there was
// nothing to summarize. A summarizer error is counted and recorded by
// fail; success clears the error and the failure count. State messages are
// never changed.
func (m *Middleware) compact(ctx context.Context, req *agent.ModelRequest, keepTokens int) (bool, error) {
	msgs := req.State.Messages()
	prev, _ := Current(req.State)
	if prev.Index > len(msgs) {
		prev = Boundary{} // stale: State was replaced, start over
	}
	k := SafeCut(msgs, prev.Index, len(msgs)-tailCount(msgs, keepTokens))
	if k <= prev.Index {
		return false, nil
	}
	// Clipped, so that a Summarizer appending to it cannot write past k.
	older := slices.Clip(msgs[prev.Index:k])
	text, err := m.summarize(ctx, req, prev.Summary, older)
	if err != nil {
		m.fail(req.State, err)
		return false, err
	}
	next := Boundary{Index: k, Summary: text, Compactions: prev.Compactions + 1}
	next.Path = m.offload(ctx, req, next.Compactions, Transcript(older))
	boundaryKey.Set(req.State, next)
	failuresKey.Delete(req.State)
	observedKey.Delete(req.State) // describes the view before this compaction
	return true, nil
}

// fail counts a failed compaction and records its cause in
// Boundary.LastError, where it stays until a compaction succeeds. The failure
// that opens the circuit breaker says so, so a host reading the Boundary can
// tell that automatic compaction has stopped.
func (m *Middleware) fail(s *state.State, cause error) {
	failures, _ := failuresKey.Get(s)
	failures++
	failuresKey.Set(s, failures)
	msg := cause.Error()
	if failures >= m.cfg.MaxFailures {
		msg = fmt.Sprintf("automatic compaction stopped after %d failures: %s", failures, msg)
	}
	boundaryKey.Update(s, func(b Boundary) Boundary {
		b.LastError = msg
		return b
	})
}

// errNoProgress is the failure of a compaction that left the boundary where
// it was, for example when the system prompt and tools alone exceed the
// trigger. Counting it keeps the trigger from firing on every turn.
var errNoProgress = errors.New("compaction made no progress")

// compactCounted is compact for the automatic and the reactive path: a
// compaction that did not move the boundary counts as a failure. It reports
// whether the boundary moved.
func (m *Middleware) compactCounted(ctx context.Context, req *agent.ModelRequest, keepTokens int) bool {
	moved, err := m.compact(ctx, req, keepTokens)
	if err == nil && !moved {
		m.fail(req.State, errNoProgress)
	}
	return moved
}

// boundedRender renders msgs for the summarizer, each block capped at
// DefaultMaxBlockChars, keeping the newest messages whose rendering fits in
// limit characters. When messages are left out, a first line
// "[earlier conversation omitted: N messages]" says how many. The kept part
// never starts with a tool result, so no result is separated from its call.
// When not even the newest message with its call fits, that part is cut from
// the front to limit and starts with "…".
func boundedRender(msgs []llm.Message, limit int) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = renderMessage(m, DefaultMaxBlockChars)
	}
	start, size := -1, 0
	for i := len(msgs) - 1; i >= 0; i-- {
		size += len(parts[i])
		if i < len(msgs)-1 {
			size += len("\n\n")
		}
		if size > limit {
			break
		}
		if i == 0 || !carriesToolResult(msgs[i]) {
			start = i
		}
	}
	if start == 0 || len(msgs) == 0 {
		return strings.Join(parts, "\n\n")
	}
	var text string
	if start > 0 {
		text = strings.Join(parts[start:], "\n\n")
	} else {
		start = SafeCut(msgs, 0, len(msgs)-1)
		text = cutFront(strings.Join(parts[start:], "\n\n"), limit)
	}
	if start == 0 {
		return text
	}
	return fmt.Sprintf("[earlier conversation omitted: %d messages]\n\n%s", start, text)
}

// cutFront returns the last limit bytes of s, at a rune boundary, starting
// with "…" when s was longer; empty when limit cannot hold the mark.
func cutFront(s string, limit int) string {
	const mark = "…"
	if len(s) <= limit {
		return s
	}
	if limit < len(mark) {
		return ""
	}
	cut := len(s) - (limit - len(mark))
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return mark + s[cut:]
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

// summarize asks the Summarizer for a summary of older, the messages after
// the summary previous: Config.Summarizer, or a ModelSummarizer built from
// Config and the request. The reply is trimmed; a blank one is an error.
func (m *Middleware) summarize(ctx context.Context, req *agent.ModelRequest, previous string, older []llm.Message) (string, error) {
	s := m.cfg.Summarizer
	if s == nil {
		ms := &ModelSummarizer{Provider: m.cfg.Provider, Model: m.cfg.Model, Prompt: m.cfg.SummaryPrompt}
		if ms.Provider == nil {
			ms.Provider = req.Provider
		}
		if ms.Model == "" {
			ms.Model = req.Request.Model
		}
		s = ms
	}
	text, err := s.Summarize(ctx, SummaryRequest{
		Messages:   older,
		Previous:   previous,
		Transcript: boundedRender(older, maxSummaryInputChars),
	})
	if err != nil {
		return "", fmt.Errorf("summarize conversation: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("summarize conversation: summarizer returned no text")
	}
	return text, nil
}

// offload saves the transcript of compaction number n to its own file and
// returns the path; empty when offload is disabled, there is no backend, the
// run has no ThreadID (threadless runs would overwrite each other's files),
// or the write failed. Each compaction gets a new file, so no file grows past
// what one read can load. A failed write is not an error: State still holds
// every message, the transcript is only a convenience for the model.
func (m *Middleware) offload(ctx context.Context, req *agent.ModelRequest, n int, transcript string) string {
	if m.cfg.DisableOffload || req.Backend == nil || req.ThreadID == "" {
		return ""
	}
	path := fmt.Sprintf("%s/%s/%d.md", HistoryDir, pathsafe.Segment(req.ThreadID), n)
	if err := req.Backend.Write(ctx, path, []byte(transcript)); err != nil {
		return ""
	}
	return path
}
