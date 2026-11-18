package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Agent runs the model/tool loop described by a Config. It is immutable after
// New and safe for concurrent Runs; everything a run changes lives in that
// run's State.
type Agent struct {
	cfg      Config
	stack    []Middleware
	registry *tool.Registry
	// runTool is the tool handler chain, built once because it holds no
	// per-run data.
	runTool ToolHandler
}

// Input starts a run.
type Input struct {
	// ThreadID identifies the conversation; it selects the backend when
	// Config.BackendFactory is set and is passed to tools.
	ThreadID string
	// Messages are appended to State's history before the first turn.
	Messages []llm.Message
	// State holds the conversation to continue; nil starts a fresh one.
	State *state.State
}

// New validates cfg, applies its defaults and builds the agent. It fails for a
// missing Provider, an invalid middleware stack (see Compose), or tools with
// clashing or illegal names across Config.Tools and every ToolProvider.
func New(cfg Config) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, errors.New("agent: Config.Provider is required")
	}
	cfg = withDefaults(cloneConfig(cfg))
	stack, err := Compose(nil, cfg.Middleware, nil)
	if err != nil {
		return nil, err
	}
	registry, err := buildRegistry(cfg.Tools, stack)
	if err != nil {
		return nil, err
	}
	return &Agent{
		cfg:      cfg,
		stack:    stack,
		registry: registry,
		runTool:  chainTool(stack, invokeTool),
	}, nil
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
// the break it sends nothing more and skips the AfterAgent hooks.
func (a *Agent) Run(ctx context.Context, in Input) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		ctx, cancel := context.WithCancel(ctx)
		r := a.newRun(ctx, in)
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
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = DefaultMaxConcurrency
	}
	if cfg.MaxToolOutputChars <= 0 {
		cfg.MaxToolOutputChars = DefaultMaxToolOutputChars
	}
	if cfg.Settlement != nil && cfg.Settlement.MaxTurns <= 0 {
		cfg.Settlement.MaxTurns = DefaultSettlementMaxTurns
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
	cfg.Rules.Allow = slices.Clone(cfg.Rules.Allow)
	cfg.Rules.Ask = slices.Clone(cfg.Rules.Ask)
	cfg.Rules.Deny = slices.Clone(cfg.Rules.Deny)
	cfg.Profile = clonePtr(cfg.Profile)
	cfg.Temperature = clonePtr(cfg.Temperature)
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
