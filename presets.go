package orchestrix

import (
	"errors"
	"fmt"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/cache"
	"github.com/aswin-kevin/orchestrix/middleware/compress"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/middleware/hitl"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/jobs"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
	"github.com/aswin-kevin/orchestrix/middleware/profiles"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/middleware/web"
	"github.com/aswin-kevin/orchestrix/tool"
)

// shellSpec is what WithShell asks for; the backend is built by New.
type shellSpec struct {
	root string
	cfg  shell.Config
	// built is the backend useShell opened; New closes it when it fails,
	// since no agent will.
	built *shell.Backend
}

// WithCheckpointer saves every thread's state with c after each state change,
// so a conversation can be continued in another process with Agent.Thread.
// With the default workspace, the thread's files are saved along with it.
// Agent.Close closes c when it implements agent.Closer, or else io.Closer;
// a checkpointer on a connection pool the host passed in (orxpg, for
// instance) leaves the pool to the host.
func WithCheckpointer(c checkpoint.Checkpointer) Option {
	return func(s *settings) { s.cfg.Checkpointer = c }
}

// WithSubagents adds subagent types the model can start with the task tool.
// They join the built-in general-purpose and fork subagents. An invalid spec
// makes New fail, even when WithoutMiddleware(subagents.Name) removes the
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

// WithInterruptOn pauses the run for a human decision before each tool that
// one of tools names, with hitl's default decisions (approve, edit, reject).
// Each entry is a tool name pattern (tool.MatchName: * matches any run of
// characters, ? exactly one), so "mcp__*" pauses every MCP tool; a pattern
// must not be empty, blank or hold a NUL byte. Use WithInterruptConfig to
// narrow the decisions or the calls that pause.
func WithInterruptOn(tools ...string) Option {
	return func(s *settings) {
		for _, name := range tools {
			s.addInterrupt(name, hitl.InterruptConfig{})
		}
	}
}

// WithInterruptConfig pauses the run for tools according to on, keyed by tool
// name pattern as in WithInterruptOn. A key equal to the tool's name wins;
// otherwise every matching pattern applies, combined fail-closed (see
// hitl.Config.InterruptOn): the call pauses when any of them would pause it,
// and only the decisions all of them allow are offered. It combines with
// WithInterruptOn; the later option wins for a key given to both.
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
// threads, and Agent.Close releases it. It cannot be combined with
// WithBackend or WithBackendFactory. Commands run with /bin/sh, so execute
// works on Unix only; on Windows every command fails to start.
//
// The shell backend runs commands as the host process, with its privileges;
// it is not a sandbox. Processes that leave a command's process group
// (setsid, daemonizing) are not killed when it ends.
//
// Middleware write transcripts and oversized outputs under
// backend.InternalDir (".orchestrix/" in root). They may contain secrets the
// conversation held; keep that directory out of version control, for
// instance with a .gitignore entry.
func WithShell(root string, cfg shell.Config) Option {
	return func(s *settings) { s.shell = &shellSpec{root: root, cfg: cfg} }
}

// WithJobs lets execute run commands in the background and adds the tools to
// follow them. It needs a backend that can start commands (a
// backend.Starter): WithShell provides one, and so does a sandbox built with
// sandbox.NewStarter, passed with WithBackend or from WithBackendFactory.
// Agent.Close stops the jobs still running (m is an agent.Closer); a host
// that does not close the agent calls m.Close(ctx) instead.
func WithJobs(m *jobs.Middleware) Option {
	return func(s *settings) { s.jobs = m }
}

// checkPresets records option combinations that cannot work.
func (s *settings) checkPresets() {
	if s.shell != nil && (s.cfg.Backend != nil || s.cfg.BackendFactory != nil) {
		s.errs = append(s.errs, errors.New("WithShell cannot be combined with WithBackend or WithBackendFactory"))
	}
	if s.jobs != nil && !s.canStartCommands() {
		s.errs = append(s.errs, errors.New("WithJobs needs a backend that can start commands: use WithShell, or WithBackend with a backend.Starter such as sandbox.NewStarter's"))
	}
}

// canStartCommands reports whether the agent's backend can run background
// jobs. A factory's backends cannot be inspected up front, so one is trusted.
func (s *settings) canStartCommands() bool {
	if s.shell != nil || s.cfg.BackendFactory != nil {
		return true
	}
	_, ok := backend.AsStarter(s.cfg.Backend)
	return ok
}

// useShell builds the shell backend and makes it cfg's workspace.
func (s *settings) useShell(cfg *agent.Config) {
	b, err := shell.New(s.shell.root, s.shell.cfg)
	if err != nil {
		s.errs = append(s.errs, fmt.Errorf("WithShell: %w", err))
		return
	}
	s.shell.built = b
	cfg.Backend = b
}

// closeBuilt releases what build opened, for a New that fails after it.
func (s *settings) closeBuilt() {
	if s.shell != nil && s.shell.built != nil {
		_ = s.shell.built.Close()
	}
}

// stackEntry is one slot of the preset's middleware stack.
type stackEntry struct {
	// name is the middleware's Name, the one WithMiddleware and
	// WithoutMiddleware match; it labels the slot.
	name string
	// option names what asked for the middleware; it prefixes a build
	// error. Empty records the error as is, for a middleware whose errors
	// already say where they come from.
	option string
	// on reports whether the stack has the middleware; nil means always.
	on func(*settings) bool
	// build returns the middleware.
	build func(*settings) (agent.Middleware, error)
}

// presetStack is the preset's middleware in its fixed order, outermost
// first (see New). summarize comes first because it rebuilds the request
// messages from State, which would discard the rewrites of any model wrapper
// before it. rubric sits early, so that BeforeStop (reverse order) consults
// the async reminder before grading, and hooks after grading. profiles is
// last, so that its suffix is the last segment and its overrides see the
// final tool list.
var presetStack = []stackEntry{
	{name: summarize.Name, option: "WithSummarize", build: func(s *settings) (agent.Middleware, error) { return summarize.New(s.summarize) }},
	{name: compress.Name, option: "WithCompress", on: func(s *settings) bool { return s.compress != nil },
		build: func(s *settings) (agent.Middleware, error) { return compress.New(*s.compress) }},
	{name: cache.Name, option: "cache", build: func(*settings) (agent.Middleware, error) { return cache.New(cache.Config{}) }},
	{name: hooks.Name, option: "WithHooks", on: func(s *settings) bool { return s.hooks != nil },
		build: func(s *settings) (agent.Middleware, error) { return hooks.New(*s.hooks) }},
	{name: hitl.Name, option: "WithInterruptOn or WithInterruptConfig", on: func(s *settings) bool { return len(s.interruptOn) > 0 },
		build: func(s *settings) (agent.Middleware, error) { return hitl.New(hitl.Config{InterruptOn: s.interruptOn}) }},
	{name: rubric.Name, option: "WithRubric", on: func(s *settings) bool { return s.rubric != nil },
		build: func(s *settings) (agent.Middleware, error) { return rubric.New(*s.rubric) }},
	{name: memory.Name, option: "WithMemory", on: func(s *settings) bool { return s.memory != nil },
		build: func(s *settings) (agent.Middleware, error) { return memory.New(*s.memory) }},
	{name: skills.Name, option: "WithSkills", on: func(s *settings) bool { return s.skills != nil }, build: buildSkills},
	{name: planmode.Name, option: "WithPlanMode", on: func(s *settings) bool { return s.planMode != nil },
		build: func(s *settings) (agent.Middleware, error) { return planmode.New(*s.planMode) }},
	{name: filesystem.Name, option: "filesystem", build: buildFilesystem},
	{name: planning.Name, option: "planning", build: func(*settings) (agent.Middleware, error) { return planning.New(planning.Config{}) }},
	{name: subagents.Name, build: func(s *settings) (agent.Middleware, error) {
		cfg := s.subagents
		cfg.Subagents = s.subagentSpecs()
		return subagents.New(cfg)
	}},
	{name: async.Name, on: func(s *settings) bool { return s.async != nil },
		build: func(s *settings) (agent.Middleware, error) { return s.async, nil }},
	{name: deferred.Name, option: "WithDeferredTools", on: func(s *settings) bool { _, ok := s.deferredConfig(); return ok },
		build: func(s *settings) (agent.Middleware, error) { cfg, _ := s.deferredConfig(); return deferred.New(cfg) }},
	{name: mcp.Name, on: func(s *settings) bool { return s.mcp != nil },
		build: func(s *settings) (agent.Middleware, error) { return s.mcp, nil }},
	{name: web.Name, option: "WithWeb", on: func(s *settings) bool { return s.web != nil },
		build: func(s *settings) (agent.Middleware, error) { return web.New(*s.web) }},
	{name: askuser.Name, option: "WithAskUser", on: func(s *settings) bool { return s.askUser != nil },
		build: func(s *settings) (agent.Middleware, error) { return askuser.New(*s.askUser) }},
	{name: jobs.Name, on: func(s *settings) bool { return s.jobs != nil },
		build: func(s *settings) (agent.Middleware, error) { return s.jobs, nil }},
	{name: profiles.Name, option: "WithProfiles", on: func(s *settings) bool { return s.profiles != nil },
		build: func(s *settings) (agent.Middleware, error) { return profiles.New(*s.profiles) }},
}

// buildSkills builds the WithSkills middleware, recording a failure so that
// checkSubagentSkills does not repeat it.
func buildSkills(s *settings) (agent.Middleware, error) {
	m, err := skills.New(*s.skills)
	if err != nil {
		s.skillsFailed = true
		return nil, err
	}
	return m, nil
}

// buildFilesystem builds the file tools, with execute when the agent can run
// commands. A backend given with WithBackend can when it is an Executor. With
// a factory the backends are unknown when tools are collected, so jobs (which
// needs commands) is the promise that execute works.
func buildFilesystem(s *settings) (agent.Middleware, error) {
	_, canExecute := backend.AsExecutor(s.cfg.Backend)
	return filesystem.New(filesystem.Config{Execute: s.shell != nil || s.jobs != nil || canExecute, Background: s.jobs != nil})
}

// baseStack builds the entries of presetStack that s turns on, in order.
func (s *settings) baseStack() []agent.Middleware {
	var out []agent.Middleware
	for _, e := range presetStack {
		if e.on == nil || e.on(s) {
			out = s.appendBuilt(out, e.option, func() (agent.Middleware, error) { return e.build(s) })
		}
	}
	return out
}

// appendBuilt appends the middleware build returns to stack, or records its
// error, prefixed with the option that asked for it (unless option is empty),
// and returns stack as is.
func (s *settings) appendBuilt(stack []agent.Middleware, option string, build func() (agent.Middleware, error)) []agent.Middleware {
	m, err := build()
	switch {
	case err == nil:
		return append(stack, m)
	case option == "":
		s.errs = append(s.errs, err)
	default:
		s.errs = append(s.errs, fmt.Errorf("%s: %w", option, err))
	}
	return stack
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
	if g, ok := tool.As[deferred.Gater](t); ok && g.Gated() {
		return true
	}
	d, ok := tool.As[deferred.Deferrer](t)
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

// WithMemory loads standing instructions, such as memory.DefaultFiles(), into
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
// is connected to. The host connects with mcp.Connect (its ConnectConfig
// sets the client info, a notification observer and a logger); Agent.Close
// closes m (an agent.Closer), or the host calls m.Close itself. A server
// configured with Defer or Gated also adds the deferred middleware, so its
// tools are found with search_tools or unlocked first.
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
// own config instead (see web.Config). A custom cfg.Fetcher owns its network
// policy instead. An invalid cfg (see web.New), such as a bad proxy URL, an
// unreadable CA file or a search backend that fails validation, makes New
// fail.
func WithWeb(cfg web.Config) Option {
	return func(s *settings) { s.web = &cfg }
}

// WithAskUser adds ask_user, with which the model puts multiple-choice
// questions to the human. Without cfg.Asker, a call pauses the run; answer it
// with askuser.Respond when resuming.
func WithAskUser(cfg askuser.Config) Option {
	return func(s *settings) { s.askUser = &cfg }
}
