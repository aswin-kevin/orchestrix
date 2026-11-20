package orchestrix

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/cache"
	"github.com/aswin-kevin/orchestrix/middleware/compress"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/middleware/hitl"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
	"github.com/aswin-kevin/orchestrix/middleware/profiles"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
	"github.com/aswin-kevin/orchestrix/middleware/web"
	"github.com/aswin-kevin/orchestrix/tool"
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
// makes New fail, even when WithoutMiddleware("subagents") removes the
// middleware afterwards.
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
// it is not a sandbox. Processes that leave a command's process group
// (setsid, daemonizing) are not killed when it ends.
//
// Middleware write transcripts and oversized outputs under
// backend.InternalDir (".orchestrix/" in root). They may contain secrets the
// conversation held; keep that directory out of version control, for
// instance with a .gitignore entry.
func WithShell(root string, opts shell.Options) Option {
	return func(s *settings) { s.shell = &shellSpec{root: root, opts: opts} }
}

// WithTasks lets execute run commands in the background and adds the tools to
// follow them. It needs a backend that can start commands (a
// backend.Starter): WithShell provides one, and so does a sandbox built with
// sandbox.NewStarter, passed with WithBackend or from WithBackendFactory. The
// host keeps m and calls its Shutdown when done, to stop jobs
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
		s.errs = append(s.errs, errors.New("WithTasks needs a backend that can start commands: use WithShell, or WithBackend with a backend.Starter such as sandbox.NewStarter's"))
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

// baseStack returns the preset's middleware in its fixed order (see New).
// summarize comes first because it rebuilds the request messages from State,
// which would discard the rewrites of any model wrapper before it.
func (s *settings) baseStack() []agent.Middleware {
	stack := []agent.Middleware{summarize.New(s.summarize)}
	if s.compress != nil {
		stack = append(stack, compress.New(*s.compress))
	}
	stack = append(stack, cache.New(cache.Config{}))
	if s.hooks != nil {
		stack = append(stack, hooks.New(*s.hooks))
	}
	if len(s.interruptOn) > 0 {
		stack = append(stack, hitl.New(hitl.Config{InterruptOn: maps.Clone(s.interruptOn)}))
	}
	if s.rubric != nil {
		// Early, so that BeforeStop (reverse order) consults the async
		// reminder before grading, and hooks after grading.
		stack = s.appendBuilt(stack, "WithRubric", func() (agent.Middleware, error) { return rubric.New(*s.rubric) })
	}
	if s.memory != nil {
		stack = s.appendBuilt(stack, "WithMemory", func() (agent.Middleware, error) { return memory.New(*s.memory) })
	}
	if s.skills != nil {
		if sk, err := skills.New(*s.skills); err != nil {
			s.errs = append(s.errs, fmt.Errorf("WithSkills: %w", err))
			s.skillsFailed = true
		} else {
			stack = append(stack, sk)
		}
	}
	if s.planMode != nil {
		stack = append(stack, planmode.New(*s.planMode))
	}
	// A backend given with WithBackend can run commands when it is an
	// Executor. With a factory the backends are unknown when tools are
	// collected, so tasks (which needs commands) is the promise that execute
	// works.
	_, canExecute := s.cfg.Backend.(backend.Executor)
	stack = append(stack,
		filesystem.New(filesystem.Config{Execute: s.shell != nil || s.tasks != nil || canExecute, Background: s.tasks != nil}),
		planning.New(planning.Config{}),
	)
	if sub := s.subagentsMiddleware(); sub != nil {
		stack = append(stack, sub)
	}
	if s.async != nil {
		stack = append(stack, s.async)
	}
	if cfg, ok := s.deferredConfig(); ok {
		stack = append(stack, deferred.New(cfg))
	}
	if s.mcp != nil {
		stack = append(stack, s.mcp)
	}
	if s.web != nil {
		stack = s.appendBuilt(stack, "WithWeb", func() (agent.Middleware, error) { return web.New(*s.web) })
	}
	if s.askUser != nil {
		stack = append(stack, askuser.New(*s.askUser))
	}
	if s.tasks != nil {
		stack = append(stack, s.tasks)
	}
	if s.profiles != nil {
		// Last, so that its suffix is the last segment and its overrides see
		// the final tool list.
		stack = s.appendBuilt(stack, "WithProfiles", func() (agent.Middleware, error) { return profiles.New(*s.profiles) })
	}
	return stack
}

// appendBuilt appends the middleware build returns to stack, or records its
// error, prefixed with the option that asked for it, and returns stack as is.
func (s *settings) appendBuilt(stack []agent.Middleware, option string, build func() (agent.Middleware, error)) []agent.Middleware {
	m, err := build()
	if err != nil {
		s.errs = append(s.errs, fmt.Errorf("%s: %w", option, err))
		return stack
	}
	return append(stack, m)
}

// deferredConfig returns the deferred middleware's configuration and whether
// the stack needs it: when WithDeferredTools asked for it, or when skill
// tools, MCP servers or WithTools tools marked with deferred.Gate or
// deferred.Defer would otherwise show schemas meant to stay hidden.
func (s *settings) deferredConfig() (deferred.Config, bool) {
	switch {
	case s.deferred != nil:
		return *s.deferred, true
	case s.skills != nil && len(s.skills.Tools) > 0, s.mcp != nil && s.mcp.HasHiddenTools(),
		slices.ContainsFunc(s.cfg.Tools, hidden):
		return deferred.Config{}, true
	}
	return deferred.Config{}, false
}

// hidden reports whether t is marked gated or deferred.
func hidden(t tool.Tool) bool {
	if g, ok := t.(deferred.Gater); ok && g.Gated() {
		return true
	}
	d, ok := t.(deferred.Deferrer)
	return ok && d.Deferred()
}

// checkSubagentSkills records the skills that subagents inheriting the
// agent's middleware ask for but stack, the agent's final middleware, does
// not define. subagents.New checks only the specs with their own Middleware;
// without this, the rest would fail at their first task call. Like every
// other spec check, it runs even when WithoutMiddleware or WithMiddleware
// removed or replaced the subagents middleware. It is skipped when WithSkills
// failed, whose error already explains the missing skills middleware.
func (s *settings) checkSubagentSkills(stack []agent.Middleware) {
	if s.skillsFailed {
		return
	}
	for _, spec := range s.subagentSpecs() {
		if spec.Middleware != nil {
			continue
		}
		if err := spec.CheckSkills(stack); err != nil {
			s.errs = append(s.errs, err)
		}
	}
}

// subagentSpecs returns the subagents of WithSubagentConfig followed by those
// of WithSubagents.
func (s *settings) subagentSpecs() []subagents.Subagent {
	return slices.Concat(s.subagents.Subagents, s.extraSubagents)
}

// subagentsMiddleware builds the subagents middleware, or returns nil after
// recording why it could not.
func (s *settings) subagentsMiddleware() agent.Middleware {
	cfg := s.subagents
	cfg.Subagents = s.subagentSpecs()
	m, err := subagents.New(cfg)
	if err != nil {
		s.errs = append(s.errs, err)
		return nil
	}
	return m
}

// WithMemory loads standing instructions, such as memory.DefaultFiles, into
// the system prompt, and with cfg.Store gives the model a long-term memory
// store. A relative path makes New fail.
func WithMemory(cfg memory.Config) Option {
	return func(s *settings) { s.memory = &cfg }
}

// WithSkills offers the model the skills of cfg: a catalog in the system
// prompt and the skill tool to load one. An unreadable source, an invalid
// programmatic skill or an unknown pinned name makes New fail; invalid skills
// found in a source are skipped and are not errors (build the middleware with
// skills.New to read their Warnings). cfg.Tools also adds the deferred
// middleware, so the gated tools stay hidden until a skill unlocks them.
func WithSkills(cfg skills.Config) Option {
	return func(s *settings) { s.skills = &cfg }
}

// WithMCP offers the tools, resources and instructions of the MCP servers m
// is connected to. The host connects with mcp.Connect and calls m.Close when
// done. A server configured with Defer or Gated also adds the deferred
// middleware, so its tools are found with search_tools or unlocked first.
func WithMCP(m *mcp.Middleware) Option {
	return func(s *settings) {
		if m == nil {
			s.errs = append(s.errs, errors.New("WithMCP: the middleware must not be nil"))
			return
		}
		s.mcp = m
	}
}

// WithPlanMode adds enter_plan_mode and exit_plan_mode: the model can switch
// the run to the read-only plan mode, and leaves it once a human approves its
// plan.
func WithPlanMode(cfg planmode.Config) Option {
	return func(s *settings) { s.planMode = &cfg }
}

// WithDeferredTools adds search_tools and call_tool and withholds the schemas
// of deferred tools (those matching cfg.Tools, and tools marked with
// deferred.Defer) until the model searches for them. Without it, the preset
// adds the middleware with a zero Config when WithSkills has Tools, WithMCP
// has a deferred or gated server, or a WithTools tool is marked with
// deferred.Gate or deferred.Defer.
func WithDeferredTools(cfg deferred.Config) Option {
	return func(s *settings) { s.deferred = &cfg }
}

// WithWeb adds web_fetch and, with cfg.Search, web_search. web_fetch refuses
// private and loopback addresses unless cfg.AllowPrivateNetworks is set, and
// uses cfg's proxy and CA settings; a search backend uses the Client of its
// own config instead (see web.Config). A bad proxy URL, an unreadable CA file
// or a search backend that fails validation makes New fail.
func WithWeb(cfg web.Config) Option {
	return func(s *settings) { s.web = &cfg }
}

// WithAskUser adds ask_user, with which the model puts multiple-choice
// questions to the human. Without cfg.Ask, a call pauses the run; answer it
// with askuser.Respond when resuming.
func WithAskUser(cfg askuser.Config) Option {
	return func(s *settings) { s.askUser = &cfg }
}
