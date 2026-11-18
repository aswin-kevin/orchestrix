package orchestrix

import (
	"errors"
	"fmt"
	"maps"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/middleware/cache"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/middleware/hitl"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
)

// shellSpec is what WithShell asks for; the backend is built by New.
type shellSpec struct {
	root string
	opts shell.Options
}

// WithCheckpointer saves every thread's state with c after each state change,
// so a conversation can be continued in another process with Agent.Thread.
// With the default workspace, the thread's files are saved along with it.
func WithCheckpointer(c checkpoint.Checkpointer) Option {
	return func(s *settings) { s.cfg.Checkpointer = c }
}

// WithSubagents adds subagent types the model can start with the task tool.
// They join the built-in general-purpose and fork subagents. An invalid spec
// makes New fail.
func WithSubagents(specs ...subagents.Subagent) Option {
	return func(s *settings) { s.extraSubagents = append(s.extraSubagents, specs...) }
}

// WithSubagentConfig replaces the default subagents configuration, for
// example to disable fork or change the nesting depth. Specs given to
// WithSubagents are appended to cfg.Subagents.
func WithSubagentConfig(cfg subagents.Config) Option {
	return func(s *settings) { s.subagents = cfg }
}

// WithSummarize replaces the default summarize configuration (context
// compaction and old tool-result clearing).
func WithSummarize(cfg summarize.Config) Option {
	return func(s *settings) { s.summarize = cfg }
}

// WithInterruptOn pauses the run for a human decision before each of the
// named tools runs, with hitl's default decisions (approve, edit, reject). Use
// WithInterruptConfig to narrow the decisions or the calls that pause.
func WithInterruptOn(tools ...string) Option {
	return func(s *settings) {
		for _, name := range tools {
			s.addInterrupt(name, hitl.InterruptConfig{})
		}
	}
}

// WithInterruptConfig pauses the run for tools according to on, keyed by tool
// name. It combines with WithInterruptOn; the later option wins for a tool
// named by both.
func WithInterruptConfig(on map[string]hitl.InterruptConfig) Option {
	return func(s *settings) {
		for name, cfg := range on {
			s.addInterrupt(name, cfg)
		}
	}
}

func (s *settings) addInterrupt(tool string, cfg hitl.InterruptConfig) {
	if s.interruptOn == nil {
		s.interruptOn = make(map[string]hitl.InterruptConfig)
	}
	s.interruptOn[tool] = cfg
}

// WithHooks runs lifecycle hooks (PreToolUse, Stop, SessionStart and so on).
func WithHooks(cfg hooks.Config) Option {
	return func(s *settings) { s.hooks = &cfg }
}

// WithShell gives the agent a shell backend rooted at root, which must be an
// existing directory, and the execute tool. The backend is shared by all
// threads. It cannot be combined with WithBackend or WithBackendFactory.
//
// The shell backend runs commands as the host process, with its privileges;
// it is not a sandbox.
func WithShell(root string, opts shell.Options) Option {
	return func(s *settings) { s.shell = &shellSpec{root: root, opts: opts} }
}

// WithTasks lets execute run commands in the background and adds the tools to
// follow them. It needs a backend that can start commands, which WithShell
// provides. The host keeps m and calls its Shutdown when done, to stop jobs
// still running.
func WithTasks(m *tasks.Middleware) Option {
	return func(s *settings) { s.tasks = m }
}

// checkPresets records option combinations that cannot work.
func (s *settings) checkPresets() {
	if s.shell != nil && (s.cfg.Backend != nil || s.cfg.BackendFactory != nil) {
		s.errs = append(s.errs, errors.New("WithShell cannot be combined with WithBackend or WithBackendFactory"))
	}
	if s.tasks != nil && !s.canStartCommands() {
		s.errs = append(s.errs, errors.New("WithTasks needs a backend that can start commands: use WithShell, or WithBackend with a backend.Starter"))
	}
}

// canStartCommands reports whether the agent's backend can run background
// jobs. A factory's backends cannot be inspected up front, so one is trusted.
func (s *settings) canStartCommands() bool {
	if s.shell != nil || s.cfg.BackendFactory != nil {
		return true
	}
	_, ok := s.cfg.Backend.(backend.Starter)
	return ok
}

// useShell builds the shell backend and makes it cfg's workspace.
func (s *settings) useShell(cfg *agent.Config) {
	b, err := shell.New(s.shell.root, s.shell.opts)
	if err != nil {
		s.errs = append(s.errs, fmt.Errorf("WithShell: %w", err))
		return
	}
	cfg.Backend = b
}

// baseStack returns the preset's middleware in its fixed order. summarize
// comes first because it rebuilds the request messages from State, which
// would discard the rewrites of any model wrapper before it.
func (s *settings) baseStack() []agent.Middleware {
	stack := []agent.Middleware{
		summarize.New(s.summarize),
		cache.New(cache.Config{}),
	}
	if s.hooks != nil {
		stack = append(stack, hooks.New(*s.hooks))
	}
	if len(s.interruptOn) > 0 {
		stack = append(stack, hitl.New(hitl.Config{InterruptOn: maps.Clone(s.interruptOn)}))
	}
	stack = append(stack,
		filesystem.New(filesystem.Config{Execute: s.shell != nil, Background: s.tasks != nil}),
		planning.New(planning.Config{}),
	)
	if sub := s.subagentsMiddleware(); sub != nil {
		stack = append(stack, sub)
	}
	if s.tasks != nil {
		stack = append(stack, s.tasks)
	}
	return stack
}

// subagentsMiddleware builds the subagents middleware, or returns nil after
// recording why it could not.
func (s *settings) subagentsMiddleware() agent.Middleware {
	cfg := s.subagents
	cfg.Subagents = append(append([]subagents.Subagent(nil), cfg.Subagents...), s.extraSubagents...)
	m, err := subagents.New(cfg)
	if err != nil {
		s.errs = append(s.errs, err)
		return nil
	}
	return m
}
