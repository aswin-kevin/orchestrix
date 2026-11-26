// Package summarize keeps long conversations inside the model's context
// window without losing anything from State.
//
// The history in State always stays complete; only the request view changes.
// Once older messages are compacted, a Boundary records how many leading
// messages a summary stands for, and every request is built as one user
// message holding the summary followed by the messages after the boundary.
// The summarized originals are also saved to the backend, one file per
// compaction under HistoryDir/<thread>, so the model can read them back; runs
// without a ThreadID skip this.
//
// Elisions (agent.ElisionsKey) apply to the messages after the boundary as
// they do in the loop's view (agent.ElidedView), and the size estimate that
// triggers compaction measures that elided view. Cuts and summaries stay in
// State space: a summary always covers the original messages, elided or not.
//
// Every request is micro-compacted as well: old, large tool results are
// replaced by a short notice and old, large tool inputs are shortened. The
// current turn is never touched: results the model has not seen yet are
// kept, and so are the tool inputs of the tool loop answering the last user
// message, which may carry signed thinking that providers validate. The
// model can ask for a compaction itself with the compact_conversation tool.
//
// Compaction also runs by itself: before a model call when the estimated
// request size passes TriggerFraction of the context window, and, once per
// call, when the provider rejects a request as too long (the retry keeps a
// smaller tail). After MaxFailures failed or fruitless compactions in a row
// the automatic trigger stops, which Boundary.LastError reports; the next
// successful compaction or the start of a new run closes the breaker again.
//
// The cut between summary and kept messages never separates a tool_result
// from its tool_use, so the view is always a valid conversation.
//
// This middleware must be the first ModelWrapper in the stack: it rebuilds
// the request messages from State, which would discard any rewrite made by a
// wrapper that ran before it.
package summarize

import (
	"context"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Defaults applied when the matching Config field is zero, and the fixed
// names and texts this package uses.
const (
	// DefaultTriggerFraction is the share of Profile.ContextWindow above
	// which a conversation is compacted automatically.
	DefaultTriggerFraction = 0.85
	// DefaultKeepFraction is the share of Profile.ContextWindow kept
	// verbatim as the recent tail when compacting.
	DefaultKeepFraction = 0.25
	// DefaultKeepToolResults is how many of the newest tool results
	// micro-compaction never clears.
	DefaultKeepToolResults = 6
	// DefaultClearResultChars is the size, in characters, above which an
	// older tool result is cleared in the view.
	DefaultClearResultChars = 2_000
	// DefaultMaxOldInputChars is the size, in characters, above which the
	// tool_use input of an older assistant message is shortened in the view.
	DefaultMaxOldInputChars = 4_000
	// DefaultMaxFailures is how many summarizer failures in a row stop
	// automatic compaction.
	DefaultMaxFailures = 3
	// DefaultSummaryMaxTokens is the reply budget of a summarizer call.
	DefaultSummaryMaxTokens = 4_096
	// CharsPerToken is the characters-per-token ratio used to estimate the
	// size of messages.
	CharsPerToken = 4
	// HistoryDir is the backend directory holding offloaded transcripts,
	// under backend.InternalDir.
	HistoryDir = backend.InternalDir + "/conversation_history"
	// ToolCompact is the name of the tool that requests a compaction.
	ToolCompact = "compact_conversation"
	// ClearedResultText replaces the content of a cleared tool result.
	ClearedResultText = "[Old tool result cleared to save context. Re-run the tool if you need it.]"
)

// DefaultSummaryPrompt is the summarizer's system prompt when
// Config.SummaryPrompt is nil.
const DefaultSummaryPrompt = `You compact the conversation of an AI agent so that it can continue its work with less context.
You receive the previous summary (possibly empty) and the conversation that followed it. Write one new summary that replaces both.
Keep everything the agent needs to continue: the user's goals, requests and constraints in their own words where it matters; decisions made and why; files, paths, commands, identifiers and values that were used or produced; errors met and how they were resolved; work completed; the current task and the next steps.
Drop pleasantries, repetition and tool output that no longer matters. Write plain prose and short lists, without preamble.`

// Config configures the summarize middleware. Zero values select the
// defaults above.
type Config struct {
	// Provider makes the summarizer calls; nil uses the request's provider.
	Provider llm.Provider
	// Model is the summarizer model; empty uses the request's model. Set it
	// when Provider is a different backend from the agent's.
	Model string
	// TriggerFraction is the share of the context window above which the
	// conversation is compacted automatically; 0 means
	// DefaultTriggerFraction.
	TriggerFraction float64
	// KeepFraction is the share of the context window kept verbatim when
	// compacting; 0 means DefaultKeepFraction.
	KeepFraction float64
	// KeepToolResults is how many of the newest tool results are never
	// cleared; 0 means DefaultKeepToolResults.
	KeepToolResults int
	// ClearResultChars is the size above which older tool results are
	// cleared in the view; 0 means DefaultClearResultChars.
	ClearResultChars int
	// MaxOldInputChars is the size above which older tool inputs are
	// shortened in the view; 0 means DefaultMaxOldInputChars.
	MaxOldInputChars int
	// MaxFailures is how many summarizer failures in a row stop automatic
	// compaction; 0 means DefaultMaxFailures.
	MaxFailures int
	// SummaryPrompt is the summarizer's system prompt; nil means
	// DefaultSummaryPrompt.
	SummaryPrompt *string
	// DisableOffload stops saving summarized messages to the backend; false
	// saves them whenever the run has a backend and a ThreadID. Runs without
	// a ThreadID never offload: they would share, and overwrite, one
	// transcript directory.
	DisableOffload bool
}

// Boundary marks how much of the history a summary replaces in the view.
type Boundary struct {
	// Index is the number of leading State messages, [0, Index), that
	// Summary represents.
	Index int `json:"index"`
	// Summary is the summarizer's text.
	Summary string `json:"summary"`
	// Path is the backend file holding the transcript of the messages this
	// compaction summarized; empty when none was written.
	Path string `json:"path,omitempty"`
	// Compactions counts the compactions that led to this boundary; it
	// numbers the transcript files. Zero for no compaction.
	Compactions int `json:"compactions,omitempty"`
	// LastError is the error of the last failed compaction attempt; empty
	// when the last attempt succeeded or none failed. A failure is recorded
	// here because compaction runs outside any tool call, where no event can
	// be forwarded, and the turn itself goes on uncompacted.
	LastError string `json:"last_error,omitempty"`
}

// BoundaryKey holds the current Boundary. It is private to the run that owns
// it.
var BoundaryKey = state.NewKey[Boundary]("summarize.boundary", state.Private[Boundary]())

// requestedKey is set by compact_conversation; the next model call compacts.
var requestedKey = state.NewKey[bool]("summarize.requested", state.Private[bool]())

// Current returns the current Boundary; false when nothing was compacted
// yet. The Boundary is returned either way, so its LastError can be read
// even before the first successful compaction.
func Current(s *state.State) (Boundary, bool) {
	b, ok := BoundaryKey.Get(s)
	return b, ok && b.Index > 0
}

// middleware is the summarize middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type middleware struct {
	cfg    Config
	prompt string
}

// New returns the summarize middleware. Put it first in the middleware list
// (see the package documentation).
func New(cfg Config) agent.Middleware {
	cfg.TriggerFraction = orDefault(cfg.TriggerFraction, DefaultTriggerFraction)
	cfg.KeepFraction = orDefault(cfg.KeepFraction, DefaultKeepFraction)
	cfg.KeepToolResults = orDefault(cfg.KeepToolResults, DefaultKeepToolResults)
	cfg.ClearResultChars = orDefault(cfg.ClearResultChars, DefaultClearResultChars)
	cfg.MaxOldInputChars = orDefault(cfg.MaxOldInputChars, DefaultMaxOldInputChars)
	cfg.MaxFailures = orDefault(cfg.MaxFailures, DefaultMaxFailures)
	m := &middleware{cfg: cfg, prompt: DefaultSummaryPrompt}
	if cfg.SummaryPrompt != nil {
		m.prompt = *cfg.SummaryPrompt
	}
	return m
}

// orDefault returns v, or def when v is not positive.
func orDefault[T int | float64](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "summarize" }

// Tools implements agent.ToolProvider.
func (m *middleware) Tools() []tool.Tool { return []tool.Tool{compactTool()} }

// BeforeAgent implements agent.BeforeAgent: a new run starts with the
// circuit breaker closed.
func (m *middleware) BeforeAgent(_ context.Context, s *state.State) error {
	if _, open := failuresKey.Get(s); open {
		failuresKey.Delete(s)
		// The error described the breaker, which is closed now.
		BoundaryKey.Update(s, func(b Boundary) Boundary {
			b.LastError = ""
			return b
		})
	}
	return nil
}

// AfterModel implements agent.AfterModel. It records how large the request
// just answered was, which is what the automatic trigger measures; a
// response without usage drops the record, as it would be stale.
func (m *middleware) AfterModel(_ context.Context, s *state.State, resp *llm.Response) error {
	u := resp.Usage
	input := u.Input + u.CacheRead + u.CacheWrite
	if input == 0 {
		observedKey.Delete(s)
		return nil
	}
	observedKey.Set(s, observed{Input: input, Messages: len(s.Messages()), Elisions: elisionsMark(s)})
	return nil
}

// WrapModel implements agent.ModelWrapper. It compacts when the model asked
// for it or the conversation nears the context window, then replaces the
// request messages with the compacted, micro-compacted view of State. A
// context overflow reported by the provider compacts harder and retries
// once, with the request as it was before the inner wrappers edited it, so
// each of their system segments and tools is sent once.
//
// A compaction that fails never fails the turn: State still holds the whole
// conversation, so the request just goes out uncompacted and the error is
// recorded in Boundary.LastError. A summarizer error counts towards the
// breaker on every path, the model's own request included; a model request with
// nothing to summarize does not. After MaxFailures failures in a row
// automatic compaction stops; a successful compaction (one the model
// requested, or a reactive one) or a new run resets the count.
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if requested, _ := requestedKey.Get(req.State); requested {
		requestedKey.Delete(req.State)
		_, _ = m.compact(ctx, req, m.keepTokens(req.Profile, fullKeepShare))
	} else if m.shouldCompact(req) {
		m.compactCounted(ctx, req, m.keepTokens(req.Profile, fullKeepShare))
	}
	base := req.Request.Messages
	req.Request.Messages = m.view(req.State, base)
	// The inner wrappers edit req in place; the retry starts from this copy.
	saved := *req
	resp, err := next(ctx, req)
	if err == nil || !llm.IsContextOverflow(err) {
		return resp, err
	}
	// The breaker does not apply here: the provider has just proven that
	// compacting is needed.
	if !m.compactCounted(ctx, req, m.keepTokens(req.Profile, reactiveKeepShare)) {
		return resp, err
	}
	*req = saved
	req.Request.Messages = m.view(req.State, base)
	return next(ctx, req)
}

// shouldCompact reports whether the request is large enough for an automatic
// compaction and the circuit breaker is closed.
func (m *middleware) shouldCompact(req *agent.ModelRequest) bool {
	if failures, _ := failuresKey.Get(req.State); failures >= m.cfg.MaxFailures {
		return false
	}
	return float64(Estimate(req.State, m.view(req.State, req.Request.Messages))) > m.cfg.TriggerFraction*float64(contextWindow(req.Profile))
}

// contextWindow is the profile's context window in tokens, or the default
// profile's when the profile leaves it unset.
func contextWindow(p llm.Profile) int {
	if p.ContextWindow > 0 {
		return p.ContextWindow
	}
	return llm.DefaultProfile().ContextWindow
}

// Shares of the configured tail a compaction keeps.
const (
	fullKeepShare = 1.0
	// reactiveKeepShare is smaller: the usual tail evidently did not fit.
	reactiveKeepShare = 0.5
)

// keepTokens is the size of the recent tail a compaction keeps verbatim: the
// configured share of the context window, scaled by share.
func (m *middleware) keepTokens(p llm.Profile, share float64) int {
	return int(share * m.cfg.KeepFraction * float64(contextWindow(p)))
}
