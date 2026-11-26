package orchestrix

import (
	"context"
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
// (WithAskUser), jobs (WithJobs), profiles (WithProfiles). deferred is
// present with WithDeferredTools, and otherwise added with a zero
// deferred.Config when WithSkills has Tools, WithMCP has a deferred or gated
// server, or a WithTools tool is marked with deferred.Gate or deferred.Defer.
// summarize must stay first: it rebuilds the request from State. rubric sits
// early because BeforeStop hooks run in reverse order: the async reminder
// about running tasks is consulted before the answer is graded, and hooks'
// Stop hooks after it. They share agent.MaxStopContinuations (see
// WithRubric). profiles is last, so its prompt suffix is the last segment
// and its overrides see the final tool list. WithMiddleware replaces a
// middleware of the same name in place and appends the others after
// profiles; WithoutMiddleware removes by name (each middleware package
// exports its name as Name, such as planning.Name).
//
// spec is a model spec string such as "anthropic:claude-sonnet-4-5" or
// "openai:gpt-4.1" (credentials and base URL come from the environment, see
// llm.ParseModel). Its model name also selects the model profile.
// WithFallback and WithFallbackConfig add backup models behind it. To use a
// ready llm.Provider instead, call NewWithProvider.
//
// Without WithBackend, WithBackendFactory or WithShell, each thread gets its
// own in-memory workspace; it lives until Agent.Close and, with
// WithCheckpointer, is saved with the thread. A run with an empty ThreadID
// gets a fresh, unshared workspace that is dropped when the run ends.
// WithMount routes path prefixes to other backends on top of any of these, for
// example memory.DefaultDir to a kv backend so memories outlive the thread.
// New never panics on bad options: every problem is returned, joined, as one
// error.
func New(spec string, opts ...Option) (*agent.Agent, error) {
	return newAgent(opts, true, func(s *settings) error { return s.resolveSpec(spec) })
}

// NewWithProvider is New for a ready llm.Provider instead of a spec string.
// It builds the same default stack. No model name is set, so no model profile
// is selected for the primary; WithFallback and
// WithFallbackConfig add backup models behind p. A nil p is an error.
func NewWithProvider(p llm.Provider, opts ...Option) (*agent.Agent, error) {
	return newAgent(opts, false, func(s *settings) error { return s.resolveProvider(p) })
}

// newAgent is the shared body of New and NewWithProvider. named reports
// whether resolve sets a model name, which the fallback chain uses to clamp
// the primary.
func newAgent(opts []Option, named bool, resolve func(*settings) error) (*agent.Agent, error) {
	s := &settings{}
	for _, opt := range opts {
		opt(s)
	}
	if err := resolve(s); err != nil {
		s.errs = append(s.errs, err)
	} else if err := s.applyFallback(named); err != nil {
		s.errs = append(s.errs, err)
	}
	s.validate()
	cfg := s.build()
	if err := errors.Join(s.errs...); err != nil {
		s.closeBuilt()
		return nil, fmt.Errorf("orchestrix: %w", err)
	}
	a, err := agent.New(cfg)
	if err != nil {
		s.closeBuilt()
		return nil, err
	}
	return a, nil
}

// resolveSpec sets the provider and model name from a spec string.
func (s *settings) resolveSpec(spec string) error {
	cfg, err := llm.ParseModel(spec)
	if err != nil {
		return err
	}
	provider, err := llm.New(cfg)
	if err != nil {
		return err
	}
	s.cfg.Provider = provider
	s.cfg.Model = cfg.Model
	return nil
}

// resolveProvider sets a ready provider on the config.
func (s *settings) resolveProvider(p llm.Provider) error {
	if p == nil {
		return errors.New("provider must not be nil")
	}
	s.cfg.Provider = p
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
		threads := newThreadMemory(mount)
		cfg.BackendFactory = threads.backend
		cfg.OnClose = append(cfg.OnClose, threads.Close)
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

// threadMemory gives every thread id its own in-memory backend and hands
// the same one back on later calls, so files survive across runs of a
// thread. A non-nil mount puts the WithMount routes over each one; a
// checkpoint still snapshots only the in-memory part. Backends are never
// evicted while the agent lives; hosts with many long-lived threads should
// supply their own factory via WithBackendFactory. The empty thread id never
// shares: each call returns a fresh backend that is not kept, so runs without
// a ThreadID cannot read each other's files. Close (an agent.Config.OnClose
// function) drops every kept backend; later calls get fresh ones that are
// not kept.
type threadMemory struct {
	mount    func(backend.Backend) backend.Backend
	mu       sync.Mutex
	backends map[string]backend.Backend // nil once closed
}

func newThreadMemory(mount func(backend.Backend) backend.Backend) *threadMemory {
	return &threadMemory{mount: mount, backends: make(map[string]backend.Backend)}
}

// backend is the agent.Config.BackendFactory.
func (m *threadMemory) backend(threadID string) backend.Backend {
	if threadID == "" {
		return m.fresh()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.backends == nil {
		return m.fresh()
	}
	b, ok := m.backends[threadID]
	if !ok {
		b = m.fresh()
		m.backends[threadID] = b
	}
	return b
}

// fresh returns a new in-memory backend under the mounts.
func (m *threadMemory) fresh() backend.Backend {
	mem, _ := memory.New(memory.Config{}) // the zero Config is valid
	var b backend.Backend = mem
	if m.mount != nil {
		b = m.mount(b)
	}
	return b
}

// Close drops every kept backend.
func (m *threadMemory) Close(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backends = nil
	return nil
}

func joinModes(modes []permission.Mode) string {
	names := make([]string, len(modes))
	for i, m := range modes {
		names[i] = string(m)
	}
	return strings.Join(names, ", ")
}

// Text wraps s as the user message that starts a run. The input has no
// ThreadID: with the default backend such a run gets a fresh, unshared
// workspace that is not kept after the run. Set Input.ThreadID to keep files
// and history across runs:
//
//	a.Run(ctx, orchestrix.Text("hello"))
func Text(s string) agent.Input {
	return agent.Input{Messages: []llm.Message{llm.UserText(s)}}
}
