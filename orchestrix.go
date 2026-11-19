package orchestrix

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
)

// New builds an agent for model with a sensible default middleware stack:
// context management (summarize), prompt caching, file tools, planning
// (write_todos) and subagents (the task tool). Options adjust the result.
//
// The stack order is fixed, outermost first: summarize, compress
// (WithCompress), cache, hooks (WithHooks), hitl (WithInterruptOn or
// WithInterruptConfig), rubric (WithRubric), memory (WithMemory), skills
// (WithSkills), planmode (WithPlanMode), filesystem, planning, subagents,
// async (WithAsync), deferred, mcp (WithMCP), web (WithWeb), askuser
// (WithAskUser), tasks (WithTasks), profiles (WithProfiles). deferred is
// present with WithDeferredTools, and otherwise added with a zero
// deferred.Config when WithSkills has Tools or WithMCP has a deferred or gated
// server. summarize must stay first: it rebuilds the request from State.
// rubric sits early because BeforeStop hooks run in reverse order: the async
// reminder about running tasks is consulted before the answer is graded, and
// hooks' Stop hooks after it. They share agent.MaxStopContinuations (see
// WithRubric). profiles is last, so its prompt
// suffix is the last segment and its overrides see the final tool list.
// WithMiddleware replaces a middleware of the same name in place and appends
// the others after profiles; WithoutMiddleware removes by name.
//
// model is either a spec string such as "anthropic:claude-sonnet-4-5" or
// "openai:gpt-4.1" (credentials and base URL come from the environment, see
// llm.ParseModel), or a ready llm.Provider. When a spec string is given, its
// model name also selects the model profile. WithFallback and
// WithFallbackConfig add backup models behind it.
//
// Without WithBackend, WithBackendFactory or WithShell, each thread gets its
// own in-memory workspace; it lives as long as the agent and, with
// WithCheckpointer, is saved with the thread. WithMount routes path prefixes
// to other backends on top of any of these, for example memory.DefaultDir to a
// kv backend so memories outlive the thread. New never panics on bad
// options: every problem is returned, joined, as one error.
func New(model any, opts ...Option) (*agent.Agent, error) {
	s := &settings{}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.resolveModel(model); err != nil {
		s.errs = append(s.errs, err)
	} else if err := s.applyFallback(model); err != nil {
		s.errs = append(s.errs, err)
	}
	s.validate()
	cfg := s.build()
	if err := errors.Join(s.errs...); err != nil {
		return nil, fmt.Errorf("orchestrix: %w", err)
	}
	return agent.New(cfg)
}

// resolveModel sets the provider and model name on the config.
func (s *settings) resolveModel(model any) error {
	switch m := model.(type) {
	case string:
		cfg, err := llm.ParseModel(m)
		if err != nil {
			return err
		}
		provider, err := llm.New(cfg)
		if err != nil {
			return err
		}
		s.cfg.Provider = provider
		s.cfg.Model = cfg.Model
	case llm.Provider:
		if m == nil {
			return errors.New("model must not be a nil llm.Provider")
		}
		s.cfg.Provider = m
	default:
		return fmt.Errorf("model must be a model spec string or an llm.Provider, got %T", model)
	}
	return nil
}

// validate records configuration mistakes that would otherwise be silently
// absorbed, so a typo fails closed instead of falling back to a default.
func (s *settings) validate() {
	if mode := s.cfg.PermissionMode; mode != "" && !mode.Valid() {
		s.errs = append(s.errs, fmt.Errorf("unknown permission mode %q, valid modes: %s", mode, joinModes(permission.Modes())))
	}
	if s.cfg.Backend != nil && s.cfg.BackendFactory != nil {
		s.errs = append(s.errs, errors.New("WithBackend and WithBackendFactory are mutually exclusive"))
	}
	s.checkPresets()
}

// build assembles the final agent.Config from the collected settings,
// recording problems in s.errs.
func (s *settings) build() agent.Config {
	cfg := s.cfg
	cfg.CacheBoundary = len(cfg.System)
	mount := s.mountWrapper()
	switch {
	case s.shell != nil:
		s.useShell(&cfg)
	case cfg.Backend == nil && cfg.BackendFactory == nil:
		cfg.BackendFactory = newThreadMemoryFactory(mount)
		cfg.SyncWorkspace = true
		mount = nil // the factory already mounts each thread's backend
	}
	applyMounts(&cfg, mount)

	stack, err := agent.Compose(s.baseStack(), cfg.Middleware, s.exclude)
	if err != nil {
		s.errs = append(s.errs, err)
	}
	cfg.Middleware = stack

	for _, patch := range s.patches {
		patch(&cfg)
	}
	s.checkSubagentSkills(cfg.Middleware)
	return cfg
}

// newThreadMemoryFactory returns a factory that gives every thread id its own
// in-memory backend and hands the same one back on later calls, so files
// survive across runs of a thread. A non-nil mount puts the WithMount routes
// over each one; a checkpoint still snapshots only the in-memory part.
// Backends are never evicted; hosts with many long-lived threads should
// supply their own factory via WithBackendFactory.
func newThreadMemoryFactory(mount func(backend.Backend) backend.Backend) func(threadID string) backend.Backend {
	var mu sync.Mutex
	backends := make(map[string]backend.Backend)
	return func(threadID string) backend.Backend {
		mu.Lock()
		defer mu.Unlock()
		b, ok := backends[threadID]
		if !ok {
			b = memory.New()
			if mount != nil {
				b = mount(b)
			}
			backends[threadID] = b
		}
		return b
	}
}

func joinModes(modes []permission.Mode) string {
	names := make([]string, len(modes))
	for i, m := range modes {
		names[i] = string(m)
	}
	return strings.Join(names, ", ")
}

// Text wraps s as the user message that starts a run:
//
//	a.Run(ctx, orchestrix.Text("hello"))
func Text(s string) agent.Input {
	return agent.Input{Messages: []llm.Message{llm.UserText(s)}}
}
