// Package subagents lets an agent delegate work to subagents through the
// task tool. Each task call starts a separate agent run that shares the
// parent's backend, streams its events into the parent's stream, and returns
// its final answer as the tool result. Its non-private state is merged back
// into the parent's when it finishes.
//
// A subagent runs in one of two modes:
//   - Isolated (every configured subagent, and the built-in general-purpose
//     one unless disabled): the run sees only the task description, under
//     its own system prompt.
//   - Fork (subagent_type Fork, unless Config.DisableFork): the run is the
//     parent's own agent continuing the parent's conversation, with the task
//     handed over as the result of its task call (ForkPrefix). System
//     prompt, tools and history prefix match the parent's, so the provider's
//     prompt cache is reused. A fork cannot delegate further, never
//     checkpoints and never syncs the workspace.
//
// Interrupts: with a parent Approver, subagents inherit it and ask it
// synchronously. Without one, a subagent that needs a decision ends
// interrupted; the parent keeps it (Suspended) and pauses the task call with
// one request that lists the subagent's pending actions and accepts approve
// and/or reject, whichever every one of those actions accepts. That
// request's Reason quotes each action's input cut short; SuspendedFor gives
// the host the subagent's full requests. Resuming the
// parent with that decision applies it to every pending action of the
// subagent and continues the subagent, which may pause again. Per-action
// decisions (edit, respond) on a subagent's requests are not available
// through the parent: a subagent whose actions accept neither approve nor
// reject is stopped, its state discarded, and the task call fails. A
// suspended subagent is dropped once its task call no longer awaits a
// decision, for instance when a new Run abandons the parent's interrupt. The
// subagent's own KindInterrupt events are not forwarded.
//
// Permissions: the task tool is read-only and allows itself, since starting
// a subagent changes nothing by itself and every tool the subagent calls is
// checked on its own. It is therefore allowed in plan and dontAsk modes too;
// a deny rule on "task" still refuses it. An isolated subagent starts in
// the stricter (permission.Stricter) of the parent's current mode (the one
// its run set with agent.SetPermissionMode, else its Config.PermissionMode)
// and its own configured mode (Subagent.PermissionMode, else the parent's
// Config.PermissionMode), so delegating never widens what may run without
// approval: a planning parent starts its subagents (and theirs) in plan
// mode, and a dontAsk parent cannot hand work to a bypassPermissions
// subagent. A fork continues the parent's State (State.Fork copies
// the mode agent.SetPermissionMode stored), so it runs in the parent's
// current mode.
//
// Skills: an isolated subagent is offered the skills its parent's run is
// offered (skills.ScopeKey), narrowed to Subagent.Skills when set, with
// Subagent.PinnedSkills injected into its prompt when set; its own subagents
// inherit that scope in turn. A fork continues the parent's State, so it keeps
// the parent's scope and loaded skills.
//
// Token usage: a subagent's model calls are reported as forwarded KindUsage
// events (with ParentToolID set) and are not included in the parent's
// Final.Usage.
package subagents

import (
	"context"
	"errors"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// ToolTask is the name of the tool that starts a subagent.
const ToolTask = agent.SubagentToolName

// Names of the built-in subagent types.
const (
	// GeneralPurpose is the built-in subagent that inherits the parent's
	// tools, middleware and model, with DefaultGeneralPurposePrompt (or
	// Config.GeneralPurposePrompt) as its system prompt.
	GeneralPurpose = "general-purpose"
	// Fork is the subagent that continues the parent's own conversation
	// (see the package documentation); no configured subagent may use the
	// name.
	Fork = "fork"
)

// Defaults applied when the matching Config field is zero.
const (
	// DefaultMaxDepth is how deep subagents may nest: a run at
	// agent.RunInfo.Depth >= MaxDepth cannot delegate. With 2, the top-level
	// agent and its subagents can delegate, their subagents cannot.
	DefaultMaxDepth = 2
	// DefaultOutputRetries is how many times a subagent whose answer fails
	// its OutputSchema is asked to correct it before the task fails.
	DefaultOutputRetries = 2
)

// Config configures the subagents middleware.
type Config struct {
	// Subagents are the subagent types the model can start, in the order the
	// task tool lists them. A spec named GeneralPurpose replaces the
	// built-in one. Nil offers only the general-purpose subagent.
	Subagents []Subagent
	// DisableGeneralPurpose removes the built-in general-purpose subagent;
	// false offers it (unless a spec replaces it).
	DisableGeneralPurpose bool
	// DisableFork removes the fork subagent; false offers it.
	DisableFork bool
	// MaxDepth bounds subagent nesting; 0 means DefaultMaxDepth. Negative
	// values are rejected.
	MaxDepth int
	// GeneralPurposePrompt is the general-purpose subagent's system prompt;
	// "" means DefaultGeneralPurposePrompt.
	GeneralPurposePrompt string
	// Prompt is the system prompt segment describing delegation. nil uses
	// DefaultPrompt; a pointer to "" disables the segment.
	Prompt *string
}

// middleware is the subagents middleware. Its configuration is fixed at New;
// children caches built subagents, which are immutable agents rather than
// per-run data, so sharing them between concurrent runs is safe.
type middleware struct {
	specs    map[string]Subagent
	maxDepth int
	prompt   string
	// fork reports whether the fork subagent is offered.
	fork bool
	task tool.Tool
	// launcher builds and starts the isolated subagents.
	launcher *Launcher
}

// New returns the subagents middleware. It fails for an invalid subagent
// spec (see Subagent), a duplicated or reserved name, a negative MaxDepth, or
// a configuration that leaves no subagent to start.
func New(cfg Config) (agent.Middleware, error) {
	if cfg.MaxDepth < 0 {
		return nil, errors.New("subagents: MaxDepth must not be negative")
	}
	list, err := ResolveSpecs(cfg)
	if err != nil {
		return nil, err
	}
	specs := make(map[string]Subagent, len(list))
	order := make([]string, len(list), len(list)+1)
	for i, s := range list {
		specs[s.Name], order[i] = s, s.Name
	}
	m := &middleware{specs: specs, maxDepth: DefaultMaxDepth, prompt: DefaultPrompt, fork: !cfg.DisableFork, launcher: NewLauncher()}
	if m.fork {
		order = append(order, Fork)
	}
	if cfg.MaxDepth > 0 {
		m.maxDepth = cfg.MaxDepth
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	m.task = m.taskTool(order)
	return m, nil
}

// BeforeAgent implements agent.BeforeAgent: it drops suspended subagents
// whose task call no longer awaits a decision (see pruneSuspended).
func (m *middleware) BeforeAgent(_ context.Context, s *state.State) error {
	pruneSuspended(s)
	return nil
}

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "subagents" }

// Tools implements agent.ToolProvider.
func (m *middleware) Tools() []tool.Tool { return []tool.Tool{m.task} }

// WrapModel implements agent.ModelWrapper. It appends the prompt after the
// host's system segments, so it stays outside the cached prefix.
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if m.prompt != "" {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt)
	}
	return next(ctx, req)
}
