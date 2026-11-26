package summarize

import (
	"context"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/internal/summary"
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
// Config.SummaryPrompt and ModelSummarizer.Prompt are empty.
const DefaultSummaryPrompt = `You compact the conversation of an AI agent so that it can continue its work with less context.
You receive the previous summary (possibly empty) and the conversation that followed it. Write one new summary that replaces both.
Keep everything the agent needs to continue: the user's goals, requests and constraints in their own words where it matters; decisions made and why; files, paths, commands, identifiers and values that were used or produced; errors met and how they were resolved; work completed; the current task and the next steps.
Drop pleasantries, repetition and tool output that no longer matters. Write plain prose and short lists, without preamble.`

// Config configures the summarize middleware. Zero values select the
// defaults above; New rejects negative numbers and fractions out of range.
type Config struct {
	// Summarizer writes the summaries; nil means a ModelSummarizer built
	// from Provider, Model and SummaryPrompt. Setting it together with any
	// of those three is an error.
	Summarizer Summarizer
	// Provider makes the summarizer calls; nil uses the request's provider.
	Provider llm.Provider
	// Model is the summarizer model; empty uses the request's model. Set it
	// when Provider is a different backend from the agent's.
	Model string
	// TriggerFraction is the share of the context window above which the
	// conversation is compacted automatically, in [0, 1]; 0 means
	// DefaultTriggerFraction. At 1 only a request larger than the whole
	// window triggers it.
	TriggerFraction float64
	// KeepFraction is the share of the context window kept verbatim when
	// compacting, in [0, 1); 0 means DefaultKeepFraction.
	KeepFraction float64
	// KeepToolResults is how many of the newest tool results are never
	// cleared; 0 means DefaultKeepToolResults. Negative is an error.
	KeepToolResults int
	// ClearResultChars is the size above which older tool results are
	// cleared in the view; 0 means DefaultClearResultChars. Negative is an
	// error.
	ClearResultChars int
	// MaxOldInputChars is the size above which older tool inputs are
	// shortened in the view; 0 means DefaultMaxOldInputChars. Negative is
	// an error.
	MaxOldInputChars int
	// MaxFailures is how many summarizer failures in a row stop automatic
	// compaction; 0 means DefaultMaxFailures. Negative is an error.
	MaxFailures int
	// SummaryPrompt is the whole system prompt of the default summarizer;
	// empty means DefaultSummaryPrompt.
	SummaryPrompt string
	// DisableOffload stops saving summarized messages to the backend; false
	// saves them whenever the run has a backend and a ThreadID. Runs without
	// a ThreadID never offload: they would share, and overwrite, one
	// transcript directory.
	DisableOffload bool
}

// Boundary marks how much of the history a summary replaces in the view.
// Its fields: Index, the number of leading State messages, [0, Index), that
// Summary represents; Summary, the summarizer's text; Path, the backend file
// holding the transcript of the messages the last compaction summarized
// (empty when none was written); Compactions, how many compactions led to
// it (zero for none); and LastError, the error of the last failed
// compaction attempt (empty when the last attempt succeeded or none
// failed), recorded there because compaction runs outside any tool call
// and the turn goes on uncompacted.
type Boundary = summary.Boundary

// boundaryKey holds the current Boundary, private to the run that owns it.
// Hosts read it with Current.
var boundaryKey = summary.Key

// requestedKey is set by compact_conversation; the next model call compacts.
var requestedKey = state.NewKey[bool]("summarize.requested", state.Private[bool]())

// Current returns the current Boundary; false when nothing was compacted
// yet. The Boundary is returned either way, so its LastError can be read
// even before the first successful compaction.
func Current(s *state.State) (Boundary, bool) {
	b, ok := boundaryKey.Get(s)
	return b, ok && b.Index > 0
}

// Name is the name of the summarize middleware.
const Name = "summarize"

// Middleware is the summarize middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type Middleware struct {
	cfg Config
}

// errSummarizerExcludes is New's error for a Summarizer set together with
// the fields of the default summarizer.
var errSummarizerExcludes = errors.New("summarize: Summarizer excludes Provider, Model and SummaryPrompt")

// New returns the summarize middleware. Put it first in the middleware list
// (see the package documentation). It returns an error when TriggerFraction
// is outside [0, 1], KeepFraction is outside [0, 1), KeepToolResults,
// ClearResultChars, MaxOldInputChars or MaxFailures is negative, or
// Summarizer is set together with Provider, Model or SummaryPrompt.
func New(cfg Config) (*Middleware, error) {
	if !(cfg.TriggerFraction >= 0 && cfg.TriggerFraction <= 1) {
		return nil, fmt.Errorf("summarize: TriggerFraction %v must be between 0 and 1", cfg.TriggerFraction)
	}
	if !(cfg.KeepFraction >= 0 && cfg.KeepFraction < 1) {
		return nil, fmt.Errorf("summarize: KeepFraction %v must be at least 0 and below 1", cfg.KeepFraction)
	}
	for _, f := range []struct {
		name string
		v    int
	}{{"KeepToolResults", cfg.KeepToolResults}, {"ClearResultChars", cfg.ClearResultChars}, {"MaxOldInputChars", cfg.MaxOldInputChars}, {"MaxFailures", cfg.MaxFailures}} {
		if f.v < 0 {
			return nil, fmt.Errorf("summarize: %s is negative", f.name)
		}
	}
	if cfg.Summarizer != nil && (cfg.Provider != nil || cfg.Model != "" || cfg.SummaryPrompt != "") {
		return nil, errSummarizerExcludes
	}
	cfg.TriggerFraction = orDefault(cfg.TriggerFraction, DefaultTriggerFraction)
	cfg.KeepFraction = orDefault(cfg.KeepFraction, DefaultKeepFraction)
	cfg.KeepToolResults = orDefault(cfg.KeepToolResults, DefaultKeepToolResults)
	cfg.ClearResultChars = orDefault(cfg.ClearResultChars, DefaultClearResultChars)
	cfg.MaxOldInputChars = orDefault(cfg.MaxOldInputChars, DefaultMaxOldInputChars)
	cfg.MaxFailures = orDefault(cfg.MaxFailures, DefaultMaxFailures)
	return &Middleware{cfg: cfg}, nil
}

// orDefault returns v, or def when v is not positive.
func orDefault[T int | float64](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Tools implements agent.ToolProvider.
func (m *Middleware) Tools() []tool.Tool { return []tool.Tool{compactTool()} }

// BeforeAgent implements agent.BeforeAgent: a new run starts with the
// circuit breaker closed.
func (m *Middleware) BeforeAgent(_ context.Context, s *state.State) error {
	if _, open := failuresKey.Get(s); open {
		failuresKey.Delete(s)
		// The error described the breaker, which is closed now.
		boundaryKey.Update(s, func(b Boundary) Boundary {
			b.LastError = ""
			return b
		})
	}
	return nil
}

// AfterModel implements agent.AfterModel. It records how large the request
// just answered was, which is what the automatic trigger measures; a
// response without usage drops the record, as it would be stale.
func (m *Middleware) AfterModel(_ context.Context, s *state.State, resp *llm.Response) error {
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
// breaker on every path, the model's own request included; a model request
// with nothing to summarize does not. After MaxFailures failures in a row
// automatic compaction stops; a successful compaction (one the model
// requested, or a reactive one) or a new run resets the count.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
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
func (m *Middleware) shouldCompact(req *agent.ModelRequest) bool {
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
func (m *Middleware) keepTokens(p llm.Profile, share float64) int {
	return int(share * m.cfg.KeepFraction * float64(contextWindow(p)))
}
