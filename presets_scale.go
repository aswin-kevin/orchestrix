package orchestrix

import (
	"errors"
	"fmt"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/composite"
	memorybackend "github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/compress"
	"github.com/aswin-kevin/orchestrix/middleware/profiles"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
)

// WithAsync adds background subagents (start_task and friends). Agent.Close
// stops the tasks still running (m is an agent.Closer); a host that does not
// close the agent calls m.Close instead. A nil m makes New fail.
// start_task needs a thread: run the agent with Input.ThreadID set. With
// WithApprover a task asks the Approver for its approvals; without one, a
// task that needs approval stops as async.StatusWaiting (see async).
func WithAsync(m *async.Middleware) Option {
	return func(s *settings) {
		if m == nil {
			s.errs = append(s.errs, errors.New("WithAsync: the middleware must not be nil"))
			return
		}
		s.async = m
	}
}

// WithCompress lets the model compress older ranges of its conversation
// (compress_context) and bring them back (restore_context). It sits right
// after summarize, whose trigger it precedes by default, so compression gets
// the first chance to make room.
func WithCompress(cfg compress.Config) Option {
	return func(s *settings) { s.compress = &cfg }
}

// WithProfiles adjusts the agent for the model it runs: a prompt suffix,
// replacement tool descriptions, and tools or middleware left out (see
// profiles.Adjustment). It is the last middleware of the stack, so its suffix
// is the last system segment and its overrides see the final tool list. An
// invalid configuration makes New fail.
//
// ExcludeMiddleware filters only the stack the agent runs, not
// Config().Middleware, so code that inspects the configured middleware does
// not see it: excluding "async" removes start_task for the matching models
// even from a coordinator, whose check that it owns the async middleware
// cannot tell. Exclusions are per model, not a sandbox: a subagent running
// another model is filtered for its own.
func WithProfiles(cfg profiles.Config) Option {
	return func(s *settings) { s.profiles = &cfg }
}

// WithRubric grades the agent's answer against cfg.Criteria before a
// top-level run ends and, when the grader finds criteria unmet, continues the
// run with its feedback (see the rubric package). Grading fails open. Missing
// criteria or an out-of-range MaxIterations make New fail.
//
// The continuations rubric asks for share agent.MaxStopContinuations (3)
// with every other BeforeStop middleware. BeforeStop middleware are
// consulted in reverse stack order and the first non-empty prompt wins: in
// the preset, middleware added with WithMiddleware first (and the async
// inbox of a background child), then the async reminder, then rubric, then
// the WithHooks Stop hooks. So grading sees the answer given after
// background tasks are dealt with, and a Stop hook runs only on an answer
// rubric passed. Once the slots are spent, no BeforeStop middleware is
// consulted again: the final answer then skips later Stop hooks, a hooks
// Stop quality gate included.
func WithRubric(cfg rubric.Config) Option {
	return func(s *settings) { s.rubric = &cfg }
}

// WithMount routes prefix to b (composite.Route) on top of whatever backend
// the agent uses: the per-thread memory default, WithBackend, a
// WithBackendFactory backend or WithShell. Repeatable. Mounting
// memory.DefaultDir on a kv or disk backend keeps memories across threads.
//
// Commands (execute) run only in the underlying backend: files under a mount
// are invisible to them. A backend installed through WithConfig (a patch of
// agent.Config.Backend or BackendFactory) is not mounted: the mounts wrap the
// backend options above, and WithConfig patches apply after them. An invalid
// or duplicate prefix ("/" included) makes New fail. With the default
// workspace and WithCheckpointer, a checkpoint saves the per-thread part
// only; mounted backends persist on their own.
//
// With WithBackend or WithShell, Agent.Close closes b too (through
// backend.AsCloser), along with the agent's backend; each backend is closed
// once. Do not pass a backend that must outlive the agent there. With the
// default per-thread workspace or WithBackendFactory, the agent does not
// close mounted backends.
func WithMount(prefix string, b backend.Backend) Option {
	return func(s *settings) { s.mounts = append(s.mounts, composite.Route{Prefix: prefix, Backend: b}) }
}

// mountWrapper validates the WithMount routes once, over a throwaway default,
// and returns the function that puts them over a backend. It returns nil when
// there are no routes, or when they are invalid, after recording why.
func (s *settings) mountWrapper() func(backend.Backend) backend.Backend {
	if len(s.mounts) == 0 {
		return nil
	}
	probe, err := memorybackend.New(memorybackend.Config{})
	if err != nil {
		s.errs = append(s.errs, fmt.Errorf("WithMount: %w", err))
		return nil
	}
	if _, err := composite.New(probe, s.mounts...); err != nil {
		s.errs = append(s.errs, fmt.Errorf("WithMount: %w", err))
		return nil
	}
	routes := slices.Clone(s.mounts)
	return func(def backend.Backend) backend.Backend {
		b, err := composite.New(def, routes...)
		if err != nil {
			// The routes are valid, so def is nil: a factory returned no
			// backend, and the run then has none, mounts included.
			return def
		}
		return b
	}
}

// applyMounts puts mount over cfg's backend: Backend is wrapped once and every
// backend the factory returns is wrapped. A nil mount changes nothing.
// Agent.Close closes the wrapped Backend through the composite's Close,
// which closes the backend it wraps and the mounted ones.
func applyMounts(cfg *agent.Config, mount func(backend.Backend) backend.Backend) {
	switch {
	case mount == nil:
	case cfg.Backend != nil:
		cfg.Backend = mount(cfg.Backend)
	case cfg.BackendFactory != nil:
		factory := cfg.BackendFactory
		cfg.BackendFactory = func(threadID string) backend.Backend { return mount(factory(threadID)) }
	}
}
