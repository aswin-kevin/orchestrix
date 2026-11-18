// Package subagents lets an agent delegate work to subagents through the
// task tool. Each task call starts a separate agent run that sees only the
// task description, shares the parent's backend, streams its events into the
// parent's stream, and returns its final answer as the tool result. A
// general-purpose subagent, which inherits the parent's whole configuration,
// is available unless disabled.
package subagents

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
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
	// Fork is reserved for a subagent that continues the parent's own
	// conversation; no configured subagent may use the name.
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
	// DisableFork removes the fork subagent; false offers it once it is
	// available.
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
	task     tool.Tool
	// children maps a childKey to its *agent.Agent.
	children sync.Map
}

// New returns the subagents middleware. It fails for an invalid subagent
// spec (see Subagent), a duplicated or reserved name, a negative MaxDepth, or
// a configuration that leaves no subagent to start.
func New(cfg Config) (agent.Middleware, error) {
	if cfg.MaxDepth < 0 {
		return nil, errors.New("subagents: MaxDepth must not be negative")
	}
	generalPrompt := cfg.GeneralPurposePrompt
	if generalPrompt == "" {
		generalPrompt = DefaultGeneralPurposePrompt
	}
	specs, order, err := validateSpecs(cfg, generalPrompt)
	if err != nil {
		return nil, err
	}
	m := &middleware{specs: specs, maxDepth: DefaultMaxDepth, prompt: DefaultPrompt}
	if cfg.MaxDepth > 0 {
		m.maxDepth = cfg.MaxDepth
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	m.task = m.taskTool(order)
	return m, nil
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
