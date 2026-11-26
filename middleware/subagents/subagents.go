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

// Name is the subagents middleware's name (agent.Middleware.Name).
const Name = "subagents"

// Middleware is the subagents middleware: it offers the task tool and the
// delegation prompt. Its configuration is fixed at New; the subagents it
// builds are cached by its Launcher, and since they are immutable agents
// rather than per-run data, concurrent runs may share one Middleware.
type Middleware struct {
	specs    map[string]Subagent
	maxDepth int
	prompt   string
	// fork reports whether the fork subagent is offered.
	fork bool
	task tool.Tool
	// launcher builds and starts the isolated subagents.
	launcher *Launcher
}

var (
	_ agent.Middleware   = (*Middleware)(nil)
	_ agent.ToolProvider = (*Middleware)(nil)
	_ agent.ModelWrapper = (*Middleware)(nil)
	_ agent.BeforeAgent  = (*Middleware)(nil)
)

// New returns the subagents middleware. It fails for an invalid subagent
// spec (see Subagent), a duplicated or reserved name, a negative MaxDepth, or
// a configuration that leaves no subagent to start.
func New(cfg Config) (*Middleware, error) {
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
	// An empty LauncherConfig cannot fail validation.
	launcher, _ := NewLauncher(LauncherConfig{})
	m := &Middleware{specs: specs, maxDepth: DefaultMaxDepth, prompt: DefaultPrompt, fork: !cfg.DisableFork, launcher: launcher}
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

// BeforeAgent implements agent.BeforeAgent: it drops the saved state of
// suspended subagents whose task call no longer awaits a decision (a new Run
// abandoned the interrupt, or the call was settled without the task tool),
// so it does not ride along in every checkpoint. A resumed run's suspended
// subagents are kept.
func (m *Middleware) BeforeAgent(_ context.Context, s *state.State) error {
	pruneSuspended(s)
	return nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Tools implements agent.ToolProvider.
func (m *Middleware) Tools() []tool.Tool { return []tool.Tool{m.task} }

// WrapModel implements agent.ModelWrapper. It appends the prompt after the
// host's system segments, so it stays outside the cached prefix.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if m.prompt != "" {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt)
	}
	return next(ctx, req)
}
