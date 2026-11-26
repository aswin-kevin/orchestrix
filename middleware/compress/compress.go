// Package compress lets the model compress older parts of its own
// conversation.
//
// The engine decides when and what: once the estimated request size passes
// TriggerFraction of the context window, it proposes the oldest settled range
// of whole user turns (a Proposal) in a <compress_request> system segment.
// The model answers by calling compress_context with a summary of that
// range. The range then becomes an elision (agent.AddElision): the request
// view shows the summary in its place, while State keeps every original
// message. restore_context removes the elision, so the range shows verbatim
// again; it needs no archive. With a backend and a thread, the originals are
// also archived under ArchiveDir/<thread>.
//
// A proposal never covers the newest KeepTurns user turns, so the current
// turn (the run's input and the tool loop answering it) is never touched.
// Only user input starts a turn: a message marked agent.InjectedMetaKey,
// such as a stop continuation the agent core appended (rubric feedback, a
// reminder, a resume prompt) or a background notification, does not.
// It starts at or after summarize's boundary, never straddles an existing
// elision, and stays clear of any call left unanswered. A proposal the model
// ignores closes after DefaultProposalTurns model turns; after MaxIgnored
// ignored proposals the run stops proposing.
//
// The summary is text the model wrote, possibly from untrusted input, so any
// <compressed> or <compress_request> tag inside it is neutralized before it
// enters the view. The <compress_request> segment itself quotes short
// excerpts (excerptChars characters) of the messages that bound the range,
// which may be user or tool text: this is the one place where such text
// enters a system segment rather than a user or tool message. The excerpts
// are kept on one line, neutralized for the compress tags and quoted as Go
// strings, and serve only to locate the range; the design keeps the request
// next to the instructions it belongs to.
//
// Put this middleware after summarize: summarize rebuilds the request
// messages from State, and compress only appends system segments and never
// rewrites the messages, nor changes elisions while a request is in flight.
package compress

import (
	"context"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Tool names, defaults applied when the matching Config field is zero, and
// the archive location.
const (
	// ToolCompress is the tool the model calls to compress a proposed range.
	ToolCompress = "compress_context"
	// ToolRestore is the tool the model calls to bring a range back.
	ToolRestore = "restore_context"
	// DefaultTriggerFraction is the share of Profile.ContextWindow above
	// which a range is proposed; below summarize's trigger, so compression
	// gets the first chance.
	DefaultTriggerFraction = 0.6
	// DefaultKeepTurns is how many of the newest user turns are never
	// proposed.
	DefaultKeepTurns = 3
	// DefaultMinRangeTokens is the smallest estimated size of a proposed
	// range; smaller ranges are not worth a summary.
	DefaultMinRangeTokens = 2_000
	// DefaultMaxSummaryChars caps, in characters, the summary
	// compress_context accepts.
	DefaultMaxSummaryChars = 8_000
	// DefaultProposalTurns is how many model turns a proposal stays open.
	DefaultProposalTurns = 2
	// DefaultMaxIgnored is how many proposals may close unanswered before
	// the run stops proposing.
	DefaultMaxIgnored = 3
	// ArchiveDir is the backend directory holding the originals of
	// compressed ranges, one file per range under the thread's directory.
	ArchiveDir = backend.InternalDir + "/compressed"
)

// Config configures the compress middleware. Zero values select the
// defaults above. Out-of-range values are not errors: a negative number
// selects the default as zero does, and a TriggerFraction of 1 or more never
// proposes a range (the request would have to exceed the whole window).
type Config struct {
	// TriggerFraction is the share of the context window above which a
	// range is proposed; 0 means DefaultTriggerFraction.
	TriggerFraction float64
	// KeepTurns is how many of the newest user turns are never proposed; 0
	// means DefaultKeepTurns.
	KeepTurns int
	// MinRangeTokens is the smallest estimated size, in tokens, of a
	// proposed range; 0 means DefaultMinRangeTokens.
	MinRangeTokens int
	// MaxSummaryChars caps the summary's length in characters; 0 means
	// DefaultMaxSummaryChars.
	MaxSummaryChars int
	// MaxIgnored is how many proposals may close unanswered in a run before
	// it stops proposing; 0 means DefaultMaxIgnored.
	MaxIgnored int
	// DisableArchive stops saving the originals to the backend; false saves
	// them whenever the run has a backend and a ThreadID (threadless runs
	// would share one directory). State keeps the originals either way.
	DisableArchive bool
	// Prompt is the system segment explaining the two tools; nil means
	// DefaultPrompt, a pointer to "" appends none. The compression request
	// itself is appended whatever Prompt is.
	Prompt *string
}

// Proposal is the range the engine asked the model to compress.
type Proposal struct {
	// ID names the range, "c<N>"; the elision recorded for it takes this ID.
	ID string `json:"id"`
	// From is the index of the range's first State message.
	From int `json:"from"`
	// To is the index just past the range's last State message.
	To int `json:"to"`
	// Turns is how many model turns the proposal has been open; zero during
	// the turn that made it.
	Turns int `json:"turns"`
}

// ProposalKey holds the open Proposal; unset when none is open. Private to
// the run, which clears it when it starts.
var ProposalKey = state.NewKey[Proposal]("compress.proposal", state.Private[Proposal]())

// Run bookkeeping. issuedKey survives runs (elision IDs must stay unique in
// the thread); the others are cleared by BeforeAgent.
var (
	// issuedKey is the highest N issued as an ID "c<N>".
	issuedKey = state.NewKey[int]("compress.issued", state.Private[int]())
	// ignoredKey counts the proposals that closed unanswered this run.
	ignoredKey = state.NewKey[int]("compress.ignored", state.Private[int]())
	// restoredKey lists the ranges restored this run, never proposed again.
	restoredKey = state.NewKey[[]span]("compress.restored", state.Private[[]span]())
	// agedKey is the Turn of the last model call that aged the proposal, so
	// a wrapper retrying the call does not age it twice.
	agedKey = state.NewKey[int]("compress.aged", state.Private[int]())
)

// middleware is immutable after New, so concurrent runs may share it.
type middleware struct {
	cfg    Config
	prompt string
}

// New returns the compress middleware. Put it after summarize in the stack
// (see the package documentation).
func New(cfg Config) agent.Middleware {
	cfg.TriggerFraction = orDefault(cfg.TriggerFraction, DefaultTriggerFraction)
	cfg.KeepTurns = orDefault(cfg.KeepTurns, DefaultKeepTurns)
	cfg.MinRangeTokens = orDefault(cfg.MinRangeTokens, DefaultMinRangeTokens)
	cfg.MaxSummaryChars = orDefault(cfg.MaxSummaryChars, DefaultMaxSummaryChars)
	cfg.MaxIgnored = orDefault(cfg.MaxIgnored, DefaultMaxIgnored)
	m := &middleware{cfg: cfg, prompt: DefaultPrompt}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
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
func (m *middleware) Name() string { return "compress" }

// Tools implements agent.ToolProvider.
func (m *middleware) Tools() []tool.Tool {
	return []tool.Tool{m.compressTool(), m.restoreTool()}
}

// BeforeAgent implements agent.BeforeAgent: a run starts with no open
// proposal, no ignored count and nothing restored. It records no elision.
func (m *middleware) BeforeAgent(_ context.Context, s *state.State) error {
	ProposalKey.Delete(s)
	ignoredKey.Delete(s)
	restoredKey.Delete(s)
	agedKey.Delete(s)
	return nil
}

// BeforeModel implements agent.BeforeModel: it drops the elisions that
// summarize's boundary has passed, before the loop builds the request from
// State, so every view of this turn sees the same elisions.
func (m *middleware) BeforeModel(_ context.Context, s *state.State) error {
	dropPassed(s)
	return nil
}

// WrapModel implements agent.ModelWrapper. It ages the open proposal, makes
// a new one when the request has grown past the trigger, and appends the
// prompt and the open request to the system segments. It never touches the
// request messages or the elisions.
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	s := req.State
	m.age(s, req.Turn)
	msgs := s.Messages()
	p, open := ProposalKey.Get(s)
	if open && (p.From < boundary(s, len(msgs)) || p.To >= len(msgs)) {
		// Summarized or replaced underneath: no longer the model's to answer.
		ProposalKey.Delete(s)
		open = false
	}
	if !open && m.shouldPropose(req) {
		if r, ok := m.selectRange(s); ok {
			p = Proposal{ID: nextID(s), From: r.From, To: r.To}
			ProposalKey.Set(s, p)
			open = true
		}
	}
	system := slices.Clone(req.Request.System)
	if m.prompt != "" {
		system = append(system, m.prompt)
	}
	if open {
		system = append(system, requestSegment(s, msgs, p))
	}
	req.Request.System = system
	return next(ctx, req)
}

// age counts one more model turn for the open proposal, closing it as
// ignored once it has been open DefaultProposalTurns turns. A second call
// in the same turn changes nothing.
func (m *middleware) age(s *state.State, turn int) {
	if last, ok := agedKey.Get(s); ok && last == turn {
		return
	}
	agedKey.Set(s, turn)
	p, open := ProposalKey.Get(s)
	if !open {
		return
	}
	p.Turns++
	if p.Turns >= DefaultProposalTurns {
		ProposalKey.Delete(s)
		ignoredKey.Update(s, func(n int) int { return n + 1 })
		return
	}
	ProposalKey.Set(s, p)
}

// shouldPropose reports whether the request is large enough for a proposal
// and the run has not ignored too many.
func (m *middleware) shouldPropose(req *agent.ModelRequest) bool {
	if ignored, _ := ignoredKey.Get(req.State); ignored >= m.cfg.MaxIgnored {
		return false
	}
	window := req.Profile.ContextWindow
	if window <= 0 {
		window = llm.DefaultProfile().ContextWindow
	}
	return float64(summarize.Estimate(req.State, req.Request.Messages)) > m.cfg.TriggerFraction*float64(window)
}

// boundary is summarize's boundary index, or 0 when there is none or it lies
// beyond the n messages of State (State replaced by the host).
func boundary(s *state.State, n int) int {
	if b, ok := summarize.Current(s); ok && b.Index <= n {
		return b.Index
	}
	return 0
}

// dropPassed removes every elision that starts before summarize's boundary:
// one wholly before it is covered by the summary, and one straddling it is
// no longer applied; both would only block new ranges. It removes such
// elisions whoever recorded them, which is harmless: ElidedView skips them
// past the boundary anyway.
func dropPassed(s *state.State) {
	b := boundary(s, len(s.Messages()))
	if b == 0 {
		return
	}
	elisions, _ := agent.ElisionsKey.Get(s)
	for _, e := range elisions {
		if e.From < b {
			noteIssued(s, e.ID)
			agent.RemoveElision(s, e.ID)
		}
	}
}
