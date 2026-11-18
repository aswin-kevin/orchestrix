// Package filesystem provides the agent's workspace tools (ls, read_file,
// write_file, edit_file, multi_edit, delete, glob, grep and, when the backend
// can run commands, execute) as a middleware over a pluggable backend.
//
// The middleware also enforces read-before-edit: a file that already exists
// can only be changed after the agent has read it, and only while it has not
// changed since. This stops the model from overwriting content it never saw.
package filesystem

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Defaults applied when the matching Config field is zero.
const (
	DefaultReadLimit      = 2000
	DefaultMaxLineChars   = 2000
	DefaultExecTimeout    = 120 * time.Second
	DefaultMaxExecTimeout = 3600 * time.Second
)

// Tool names.
const (
	ToolLs        = "ls"
	ToolReadFile  = "read_file"
	ToolWriteFile = "write_file"
	ToolEditFile  = "edit_file"
	ToolMultiEdit = "multi_edit"
	ToolDelete    = "delete"
	ToolGlob      = "glob"
	ToolGrep      = "grep"
	ToolExecute   = "execute"
)

// allTools lists every tool name this middleware can provide.
var allTools = []string{
	ToolLs, ToolReadFile, ToolWriteFile, ToolEditFile, ToolMultiEdit,
	ToolDelete, ToolGlob, ToolGrep, ToolExecute,
}

// ReadKey records, per path, the modification time of the file as the agent
// last read or wrote it. It is private to the run that owns it.
var ReadKey = state.NewKey[map[string]time.Time]("filesystem.read", state.Private[map[string]time.Time]())

// Config configures the filesystem middleware.
type Config struct {
	// Backend is the workspace the tools operate on. When nil, each call uses
	// the agent's own backend (tool.Call.Backend).
	Backend backend.Backend

	// Tools is an allowlist of tool names; nil provides all of them. When set
	// it must include "read_file", because read-before-edit depends on it.
	Tools []string

	// Descriptions overrides the model-facing description of a tool by name.
	Descriptions map[string]string

	// Prompt is the system prompt segment describing the tools. nil uses
	// DefaultPrompt; a pointer to "" disables the segment.
	Prompt *string

	// ReadLimit is the default number of lines read_file returns; 0 means
	// DefaultReadLimit.
	ReadLimit int

	// MaxLineChars truncates longer lines in read_file and grep output; 0
	// means DefaultMaxLineChars.
	MaxLineChars int

	// OffloadChars is the size above which the output of other middleware's
	// tools is saved to the backend and replaced by a head and a pointer; 0
	// means DefaultOffloadChars.
	OffloadChars int

	// ExecTimeout is the timeout execute uses when the model gives none and
	// MaxExecTimeout is the most it may ask for; 0 means DefaultExecTimeout
	// and DefaultMaxExecTimeout.
	ExecTimeout, MaxExecTimeout time.Duration

	// Execute registers the execute tool even when Backend is nil. Tools are
	// collected once, before any per-thread backend exists, so with a nil
	// Backend the middleware cannot tell whether the agent's backend can run
	// commands. Setting Execute is the caller's promise that it can; a call
	// on a backend that cannot returns an error result. With a non-nil
	// Backend, execute is registered exactly when it implements
	// backend.Executor.
	Execute bool
}

// middleware is the filesystem middleware. It is immutable after New, so it
// is safe to share between concurrent runs.
type middleware struct {
	cfg      Config
	prompt   string
	excluded []string // known tools left out by the allowlist
}

// New returns the filesystem middleware. It panics on an invalid Config (an
// allowlist that omits read_file or names an unknown tool), since that is a
// programming error.
func New(cfg Config) agent.Middleware {
	m := &middleware{cfg: withDefaults(cfg), prompt: DefaultPrompt}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	if cfg.Tools != nil {
		validateAllowlist(cfg.Tools)
		for _, name := range allTools {
			if !slices.Contains(cfg.Tools, name) {
				m.excluded = append(m.excluded, name)
			}
		}
	}
	return m
}

func withDefaults(cfg Config) Config {
	if cfg.ReadLimit <= 0 {
		cfg.ReadLimit = DefaultReadLimit
	}
	if cfg.MaxLineChars <= 0 {
		cfg.MaxLineChars = DefaultMaxLineChars
	}
	if cfg.OffloadChars <= 0 {
		cfg.OffloadChars = DefaultOffloadChars
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = DefaultExecTimeout
	}
	if cfg.MaxExecTimeout <= 0 {
		cfg.MaxExecTimeout = DefaultMaxExecTimeout
	}
	return cfg
}

func validateAllowlist(names []string) {
	if !slices.Contains(names, ToolReadFile) {
		panic("filesystem: Config.Tools must include " + ToolReadFile)
	}
	for _, name := range names {
		if !slices.Contains(allTools, name) {
			panic(fmt.Sprintf("filesystem: unknown tool %q in Config.Tools", name))
		}
	}
}

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "filesystem" }

// Tools implements agent.ToolProvider.
func (m *middleware) Tools() []tool.Tool {
	tools := []tool.Tool{
		m.lsTool(), m.readFileTool(), m.writeFileTool(), m.editFileTool(),
		m.multiEditTool(), m.deleteTool(), m.globTool(), m.grepTool(),
	}
	if m.registersExecute() {
		tools = append(tools, m.executeTool())
	}
	return slices.DeleteFunc(tools, func(t tool.Tool) bool {
		return slices.Contains(m.excluded, t.Name())
	})
}

// WrapModel implements agent.ModelWrapper. It appends the tool guidance after
// the host's system segments, so it stays outside the cached prefix, and
// drops schemas of tools the allowlist excludes. With a configured backend it
// also moves oversized user text out of the request (see offloadLargeMessages).
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if m.prompt != "" {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt)
	}
	if len(m.excluded) > 0 {
		req.Request.Tools = slices.DeleteFunc(slices.Clone(req.Request.Tools), func(s llm.ToolSchema) bool {
			return slices.Contains(m.excluded, s.Name)
		})
	}
	// A model request carries no per-call backend, so only a configured one
	// can receive offloaded messages.
	if m.cfg.Backend != nil {
		req.Request.Messages = m.offloadLargeMessages(ctx, m.cfg.Backend, req.Request.Messages)
	}
	return next(ctx, req)
}

// description returns the override for name, or the built-in text.
func (m *middleware) description(name, builtin string) string {
	if d, ok := m.cfg.Descriptions[name]; ok {
		return d
	}
	return builtin
}

// backendFor picks the configured backend, else the agent's.
func (m *middleware) backendFor(c *tool.Call) (backend.Backend, error) {
	switch {
	case m.cfg.Backend != nil:
		return m.cfg.Backend, nil
	case c != nil && c.Backend != nil:
		return c.Backend, nil
	}
	return nil, errNoBackend
}
