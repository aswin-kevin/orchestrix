// Package coordinator provides a coordinator agent, which breaks a request
// into subtasks, delegates them to worker subagents and combines their
// results, and RunParallel, which runs many agent runs at once with bounded
// concurrency.
package coordinator

import (
	"errors"
	"fmt"
	"sync"

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
}

// Coordinator is an agent that plans, delegates to its workers (task for
// work needed now, start_task for parallel work) and synthesizes their
// results. Run it with an Input.ThreadID: start_task refuses to start
// workers in a run without one. Call Shutdown when done, or Close (of the
// embedded Agent), which stops the workers too and releases the agent's
// other resources.
type Coordinator struct {
	// Agent is the coordinating agent; Run, Thread, Resume and Close come
	// from it.
	*agent.Agent
	// tasks runs the background workers; the Coordinator owns it.
	tasks    *async.Middleware
	shutdown sync.Once
}

// New builds it with orchestrix.New(spec, …): the system prompt, subagents
// with Workers only (no general-purpose, no fork), async with the same
// Workers (MaxRunning = MaxConcurrency), then opts (later options win).
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
		return nil, errors.New("coordinator: MaxConcurrency must not be negative")
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
		tasks.Shutdown()
		return nil, fmt.Errorf("coordinator: %w", err)
	}
	return &Coordinator{Agent: a, tasks: tasks}, nil
}

// errReplacedAsync reports opts that took the Coordinator's async middleware
// out of the stack: start_task would then not run the workers, or would run
// tasks Shutdown does not stop.
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

// Shutdown stops every background worker still running and waits for them
// to end, up to async.DefaultStopWait. Workers cannot be started in the
// background afterwards. It is safe to call more than once and from several
// goroutines; later calls return once the first has finished.
func (c *Coordinator) Shutdown() {
	c.shutdown.Do(c.tasks.Shutdown)
}
