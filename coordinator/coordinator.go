package coordinator

import (
	"context"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
)

// DefaultMaxConcurrency is how many background workers a Coordinator runs at
// once per thread, and how many jobs RunParallel runs at once, when the
// matching field is zero.
const DefaultMaxConcurrency = 4

// Config configures a Coordinator.
type Config struct {
	// Workers are the subagents the coordinator delegates to; at least one
	// is required.
	Workers []subagents.Subagent
	// Prompt is the whole system prompt; "" means DefaultPrompt.
	Prompt string
	// MaxConcurrency is how many background workers may run at once per
	// thread; 0 means DefaultMaxConcurrency. Negative values are rejected.
	MaxConcurrency int
	// Runner starts the background workers (async.Config.Runner); the
	// request's SubagentType is the worker's name. nil runs them as
	// subagents in this process.
	Runner async.Runner
	// OnSettled is told when an episode of a background worker stops: it
	// ended, or it now waits for a human decision, which the host gives with
	// Coordinator.Resolve. It is async.Config.OnSettled of the coordinator's
	// async middleware, with the same contract. nil calls nothing.
	OnSettled func(ctx context.Context, threadID string, rec async.TaskRecord)
	// OnEvent receives every event of every episode of a background worker
	// the built-in runner runs. It is async.Config.OnEvent of the
	// coordinator's async middleware, with the same contract. nil calls
	// nothing.
	OnEvent func(threadID, taskID string, ev agent.Event)
}

// Coordinator is an agent that plans, delegates to its workers (task for
// work needed now, start_task for parallel work) and synthesizes their
// results. Run it with an Input.ThreadID: start_task refuses to start
// workers in a run without one. Call Close (of the embedded Agent) when
// done: it stops the background workers still running and releases the
// agent's other resources.
type Coordinator struct {
	// Agent is the coordinating agent; Run, Thread, Resume and Close come
	// from it.
	*agent.Agent
	// tasks is the async middleware running the background workers.
	tasks *async.Middleware
}

// Resolve answers a background worker that waits for a human decision: the
// task taskID started by a run of threadID, as an OnSettled report with
// rec.Pending set announced it. It is async.Middleware.Resolve of the
// coordinator's async middleware, with the same contract and errors
// (async.ErrUnknownTask, async.ErrNotWaiting, async.ErrCannotReattach, ...):
// the decisions answer the worker's pending actions, and it goes on in a new
// episode whose end is reported to OnSettled and to the thread's next run.
func (c *Coordinator) Resolve(ctx context.Context, threadID, taskID string, decisions []agent.Decision) error {
	return c.tasks.Resolve(ctx, threadID, taskID, decisions)
}

// New builds it with orchestrix.New(spec, …): the system prompt, subagents
// with Workers only (no general-purpose, no fork), async with the same
// Workers (MaxRunning = MaxConcurrency, and Runner), then opts (later options win).
// A worker leaving Middleware nil
// inherits the coordinator's stack, as with the task tool. Specs given with
// orchestrix.WithSubagents join the task tool only, not start_task. A host
// passing its own orchestrix.WithSubagentConfig replaces the coordinator's,
// so it must re-pass Workers (as Subagents), DisableGeneralPurpose and
// DisableFork to keep the task tool limited to the workers.
//
// New fails without Workers, for a negative MaxConcurrency, an invalid
// worker spec, when opts replace or remove the async middleware (with
// orchestrix.WithAsync, orchestrix.WithMiddleware or
// orchestrix.WithoutMiddleware), or for anything orchestrix.New rejects.
// The check reads the configured middleware, so it cannot see a profiles
// adjustment (orchestrix.WithProfiles) whose ExcludeMiddleware names "async":
// that removes start_task for the matching models when the agent is built, and
// the coordinator is then left with the task tool only.
func New(spec string, cfg Config, opts ...orchestrix.Option) (*Coordinator, error) {
	return build(cfg, func(all []orchestrix.Option) (*agent.Agent, error) {
		return orchestrix.New(spec, all...)
	}, opts)
}

// NewWithProvider is New for a ready llm.Provider instead of a spec string.
// A nil p is an error.
func NewWithProvider(p llm.Provider, cfg Config, opts ...orchestrix.Option) (*Coordinator, error) {
	return build(cfg, func(all []orchestrix.Option) (*agent.Agent, error) {
		return orchestrix.NewWithProvider(p, all...)
	}, opts)
}

// build is the shared body of New and NewWithProvider; newAgent builds the
// agent from the coordinator's options followed by opts.
func build(cfg Config, newAgent func([]orchestrix.Option) (*agent.Agent, error), opts []orchestrix.Option) (*Coordinator, error) {
	if len(cfg.Workers) == 0 {
		return nil, errors.New("coordinator: at least one worker is required")
	}
	if cfg.MaxConcurrency < 0 {
		return nil, errors.New("coordinator: MaxConcurrency is negative")
	}
	prompt := cfg.Prompt
	if prompt == "" {
		prompt = DefaultPrompt
	}
	maxRunning := cfg.MaxConcurrency
	if maxRunning == 0 {
		maxRunning = DefaultMaxConcurrency
	}
	tasks, err := async.New(async.Config{
		Subagents:             cfg.Workers,
		DisableGeneralPurpose: true,
		MaxRunning:            maxRunning,
		Runner:                cfg.Runner,
		OnSettled:             cfg.OnSettled,
		OnEvent:               cfg.OnEvent,
	})
	if err != nil {
		return nil, fmt.Errorf("coordinator: %w", err)
	}
	base := []orchestrix.Option{
		orchestrix.WithSystemPrompt(prompt),
		orchestrix.WithSubagentConfig(subagents.Config{
			Subagents:             cfg.Workers,
			DisableGeneralPurpose: true,
			DisableFork:           true,
		}),
		orchestrix.WithAsync(tasks),
	}
	all := append(base, opts...)
	a, err := newAgent(all)
	if err == nil && !owns(a, tasks) {
		err = errReplacedAsync
	}
	if err != nil {
		_ = tasks.Close(context.Background()) // nothing ran: there is no worker to wait for
		return nil, fmt.Errorf("coordinator: %w", err)
	}
	return &Coordinator{Agent: a, tasks: tasks}, nil
}

// errReplacedAsync reports opts that took the Coordinator's async middleware
// out of the stack: start_task would then not run the workers, or would run
// tasks Close does not stop.
var errReplacedAsync = errors.New("opts must not replace or remove the async middleware (configure workers with Config)")

// owns reports whether the middleware named "async" in a's stack is tasks.
func owns(a *agent.Agent, tasks *async.Middleware) bool {
	for _, m := range a.Config().Middleware {
		if m.Name() == tasks.Name() {
			return m == agent.Middleware(tasks)
		}
	}
	return false
}
