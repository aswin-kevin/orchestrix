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
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
)

// DefaultMaxConcurrency is how many background workers a Coordinator runs at
// once, and how many jobs RunParallel runs at once, when the matching field
// is zero.
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
// results. Call Shutdown when done.
type Coordinator struct {
	*agent.Agent
	// tasks runs the background workers; the Coordinator owns it.
	tasks    *async.Middleware
	shutdown sync.Once
}

// New builds it with orchestrix.New(model, …): the system prompt, subagents
// with Workers only (no general-purpose, no fork), async with the same
// Workers (MaxRunning = MaxConcurrency), then opts (later options win).
// model is what orchestrix.New accepts. A worker leaving Middleware nil
// inherits the coordinator's stack, as with the task tool. Specs given with
// orchestrix.WithSubagents join the task tool only, not start_task.
//
// New fails without Workers, for a negative MaxConcurrency, an invalid
// worker spec, or anything orchestrix.New rejects.
func New(model any, cfg Config, opts ...orchestrix.Option) (*Coordinator, error) {
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
	a, err := orchestrix.New(model, append(base, opts...)...)
	if err != nil {
		tasks.Shutdown()
		return nil, fmt.Errorf("coordinator: %w", err)
	}
	return &Coordinator{Agent: a, tasks: tasks}, nil
}

// Shutdown stops every background worker still running and waits for them
// to end, up to async.DefaultStopWait. Workers cannot be started in the
// background afterwards. It is safe to call more than once and from several
// goroutines; later calls return once the first has finished.
func (c *Coordinator) Shutdown() {
	c.shutdown.Do(c.tasks.Shutdown)
}
