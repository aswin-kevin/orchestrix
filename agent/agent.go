package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Agent runs the model/tool loop described by a Config. It is immutable after
// New, apart from being closed (Close), and safe for concurrent Runs;
// everything a run changes lives in that run's State. Create one with New;
// the zero value is not usable.
type Agent struct {
	cfg Config
	// logger is Config.Logger, or a discarding logger when that is nil.
	logger *slog.Logger
	stack  []Middleware
	// resolvers are the stack's OrphanResolvers, in stack order.
	resolvers []orphanResolver
	registry  *tool.Registry
	// dynamic are the stack's DynamicToolProviders, in stack order; see
	// tools.go.
	dynamic []DynamicToolProvider
	// runTool is the tool handler chain, built once because it holds no
	// per-run data.
	runTool ToolHandler
	// outputPrompt is the system segment built from Config.OutputSchema,
	// "" when there is none; see output.go.
	outputPrompt string

	// closed is set by Close; runs started afterwards do not start.
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// Input starts a run. Its fields override, for this run only, what the
// agent's Config would otherwise decide.
type Input struct {
	// ThreadID identifies the conversation; it selects the backend when
	// Config.BackendFactory is set and is passed to tools and model
	// wrappers. Empty is allowed and is passed on as is; the run is not
	// checkpointed, and orchestrix.New's default factory gives it a fresh,
	// unshared workspace that is dropped after the run.
	ThreadID string
	// Messages are appended to State's history before the first turn. A
	// resumed run takes none (see ResumeWith).
	Messages []llm.Message
	// State holds the conversation to continue; nil starts a fresh one.
	// ResumeWith requires it.
	State *state.State
	// Version is the checkpoint version State was loaded at (from
	// Checkpointer.Load or a previous Final.Version); the run's first save
	// expects it. The zero Version means the thread has no checkpoint yet.
	// It matters only for a persisted run (Config.Checkpointer).
	Version checkpoint.Version
	// Backend, when non-nil, is the backend this run uses instead of
	// BackendFactory(ThreadID) or Config.Backend; nil keeps those.
	Backend backend.Backend
	// AgentName, when non-empty, replaces Config.Name as the name the run
	// reports in Event.Agent, tool.Call.AgentID and RunInfo.AgentName; empty
	// keeps Config.Name.
	AgentName string
	// Depth, when positive, is the least RunInfo.Depth of the run: a run
	// started from a tool call's context is nested by that call (its Depth
	// is the parent's plus one), and Depth marks as nested a run that
	// continues a nested one outside any tool call, such as a background
	// task resumed after a restart, so that middleware treats it as a
	// subagent (nesting limits, plan mode, the guard). The run's Depth is
	// the larger of the two; zero or less leaves it as ctx gives it.
	// ParentToolID stays as ctx gives it.
	Depth int
	// Ephemeral keeps the run out of Config.Checkpointer even when ThreadID
	// is set; nested (subagent) runs set it because their parent's thread is
	// the one that is saved. False saves as Config.Checkpointer describes.
	Ephemeral bool

	// onEnd, when set, is called with the run's Final on every way the run
	// ends, including a consumer leaving early, before the event stream
	// closes. Thread uses it to adopt a run whose KindFinal nobody read.
	onEnd func(*Final)
}

// New validates cfg, applies its defaults and builds the agent. It fails for a
// missing Provider, an unknown non-empty PermissionMode, an invalid middleware
// stack (see Compose), tools with clashing or illegal names across
// Config.Tools and every ToolProvider, or an OutputSchema whose top-level
// "type" is not "object" or that cannot be encoded as JSON.
//
// The composed stack is then filtered by every MiddlewareExcluder for
// Config.Model; New fails when an excluder names a middleware that is
// Required. Only the stack is filtered: Config().Middleware stays
// unfiltered, so a child built from it re-filters for its own model.
// Every Binder of the filtered stack is then bound to the new agent, in
// stack order; New fails with the first error.
func New(cfg Config) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, errors.New("agent: Config.Provider is required")
	}
	if mode := cfg.PermissionMode; mode != "" && !mode.Valid() {
		return nil, fmt.Errorf("agent: unknown permission mode %q", mode)
	}
	if cfg.OutputRetries < NoOutputRetries {
		return nil, fmt.Errorf("agent: Config.OutputRetries %d is invalid", cfg.OutputRetries)
	}
	if cfg.EscalateMaxTokens < 0 {
		return nil, fmt.Errorf("agent: Config.EscalateMaxTokens %d is negative", cfg.EscalateMaxTokens)
	}
	if badPromptVerbs(cfg.BudgetNudge, 0) {
		return nil, errors.New("agent: Config.BudgetNudge has bad format verbs")
	}
	if badPromptVerbs(cfg.InvalidOutputPrompt, errors.New("x")) {
		return nil, errors.New("agent: Config.InvalidOutputPrompt has bad format verbs")
	}
	cfg = withDefaults(cloneConfig(cfg))
	format, err := outputPrompt(cfg.OutputPrompt, cfg.OutputSchema)
	if err != nil {
		return nil, err
	}
	stack, err := Compose(nil, cfg.Middleware, nil)
	if err != nil {
		return nil, err
	}
	if stack, err = filterExcluded(stack, cfg.Model); err != nil {
		return nil, err
	}
	registry, err := buildRegistry(cfg.Tools, stack)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		cfg:          cfg,
		logger:       loggerOrDiscard(cfg.Logger),
		stack:        stack,
		registry:     registry,
		dynamic:      dynamicProviders(stack),
		resolvers:    orphanResolvers(stack),
		runTool:      chainTool(stack, invokeTool),
		outputPrompt: format,
	}
	for _, mw := range stack {
		if b, ok := mw.(Binder); ok {
			if err := b.Bind(a); err != nil {
				return nil, fmt.Errorf("agent: middleware %q: %w", mw.Name(), err)
			}
		}
	}
	return a, nil
}

// Config returns a copy of the agent's configuration with defaults applied.
func (a *Agent) Config() Config {
	return cloneConfig(a.cfg)
}

// Run starts a run and streams its events. KindFinal is always the last
// event; it is yielded together with Final.Err, so the error is non-nil only
// for a run that failed. Every other event is yielded with a nil error.
//
// Breaking out of the loop (or panicking in it) cancels the run: in-flight
// model and tool calls see their context cancelled, and the iterator returns
// once they have stopped, so no goroutine outlives it. Once the run notices
// the break it sends nothing more and skips the AfterAgent hooks and the
// end-of-run checkpoint; the checkpoints saved before the break remain.
//
// When in.State holds a run that ended interrupted, Run abandons that
// interrupt and starts afresh: the calls it left unanswered are answered from
// their saved results or an OrphanResolver, else the model sees them as
// interrupted. Use Resume to continue it instead.
//
// After Close, the stream is a single KindFinal with ReasonInternalError
// wrapping ErrClosed.
func (a *Agent) Run(ctx context.Context, in Input) iter.Seq2[Event, error] {
	return a.stream(ctx, in, nil)
}

// name returns the name a run started with in reports.
func (a *Agent) name(in Input) string {
	if in.AgentName != "" {
		return in.AgentName
	}
	return a.cfg.Name
}

// stream starts a run, which first finishes the interrupted turn when resume
// is set, and streams its events as described on Run.
func (a *Agent) stream(ctx context.Context, in Input, resume *resumption) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if a.closed.Load() {
			yield(a.closedEvent(in), ErrClosed)
			return
		}
		ctx, cancel := context.WithCancel(ctx)
		r := a.newRun(ctx, in, resume)
		go r.drive(in.Messages)
		// After a normal finish this only releases the context.
		defer r.abandon(cancel)
		for ev := range r.events {
			var err error
			if ev.Kind == KindFinal {
				err = ev.Final.Err
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// withDefaults fills every zero field that has a documented default.
func withDefaults(cfg Config) Config {
	if cfg.Name == "" {
		cfg.Name = DefaultAgentName
	}
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = DefaultMaxTurns
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = DefaultMaxConcurrency
	}
	if cfg.MaxToolOutputChars <= 0 {
		cfg.MaxToolOutputChars = DefaultMaxToolOutputChars
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	if cfg.Settlement != nil && cfg.Settlement.MaxTurns <= 0 {
		cfg.Settlement.MaxTurns = DefaultSettlementMaxTurns
	}
	if cfg.BudgetNudge == "" {
		cfg.BudgetNudge = defaultBudgetNudge
	}
	if cfg.ResumePrompt == "" {
		cfg.ResumePrompt = defaultResumePrompt
	}
	if cfg.OutputPrompt == "" {
		cfg.OutputPrompt = defaultOutputPrompt
	}
	if cfg.InvalidOutputPrompt == "" {
		cfg.InvalidOutputPrompt = defaultInvalidOutputPrompt
	}
	if cfg.OutputRetries == 0 {
		cfg.OutputRetries = DefaultOutputRetries
	}
	if cfg.Profile == nil {
		profile := llm.LookupProfile(cfg.Model)
		cfg.Profile = &profile
	}
	return cfg
}

// cloneConfig copies cfg deeply enough that neither copy can change the
// other through a shared slice, map or pointer. The values inside (tools,
// middleware, the provider) are shared by design.
func cloneConfig(cfg Config) Config {
	cfg.System = slices.Clone(cfg.System)
	cfg.Tools = slices.Clone(cfg.Tools)
	cfg.Middleware = slices.Clone(cfg.Middleware)
	cfg.OnClose = slices.Clone(cfg.OnClose)
	cfg.Rules.Allow = slices.Clone(cfg.Rules.Allow)
	cfg.Rules.Ask = slices.Clone(cfg.Rules.Ask)
	cfg.Rules.Deny = slices.Clone(cfg.Rules.Deny)
	cfg.Profile = clonePtr(cfg.Profile)
	cfg.Temperature = clonePtr(cfg.Temperature)
	cfg.OutputSchema = maps.Clone(cfg.OutputSchema)
	if cfg.Settlement != nil {
		settlement := *cfg.Settlement
		settlement.DisabledTools = slices.Clone(settlement.DisabledTools)
		settlement.PromptByReason = maps.Clone(settlement.PromptByReason)
		cfg.Settlement = &settlement
	}
	return cfg
}

// clonePtr returns a pointer to a copy of *p, or nil for a nil p.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// buildRegistry registers the configured tools followed by those contributed
// by middleware, in stack order.
func buildRegistry(tools []tool.Tool, stack []Middleware) (*tool.Registry, error) {
	registry, err := tool.NewRegistry(tools...)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	for _, mw := range stack {
		provider, ok := mw.(ToolProvider)
		if !ok {
			continue
		}
		for _, t := range provider.Tools() {
			if err := registry.Add(t); err != nil {
				return nil, fmt.Errorf("agent: middleware %q: %w", mw.Name(), err)
			}
		}
	}
	return registry, nil
}
