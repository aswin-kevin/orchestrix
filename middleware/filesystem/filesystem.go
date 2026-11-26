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

// readKey records, per path, the modification time of the file as the agent
// last read or wrote it. It is private to the run that owns it.
var readKey = state.NewKey[map[string]time.Time]("filesystem.read", state.Private[map[string]time.Time]())

// ReadAt returns the modification time of path as the agent last read or
// wrote it in this run, and false when it has not.
func ReadAt(s *state.State, path string) (time.Time, bool) {
	read, _ := readKey.Get(s)
	t, ok := read[path]
	return t, ok
}

// Config configures the filesystem middleware.
type Config struct {
	// Backend is the workspace the tools operate on. When nil, each call uses
	// the agent's own backend (tool.Call.Backend).
	Backend backend.Backend

	// Tools is an allowlist of tool names; nil provides all of them. When set
	// it must include "read_file", because read-before-edit depends on it,
	// and name only known tools; New returns an error otherwise.
	Tools []string

	// Descriptions overrides the model-facing description of a tool by name.
	// Every key must be a tool this middleware can provide.
	Descriptions map[string]string

	// Prompt is the system prompt segment describing the tools. nil uses
	// DefaultPrompt; a pointer to "" disables the segment.
	Prompt *string

	// ReadLimit is the default number of lines read_file returns; 0 means
	// DefaultReadLimit. Negative is an error.
	ReadLimit int

	// MaxLineChars truncates longer lines in read_file and grep output; 0
	// means DefaultMaxLineChars. Negative is an error.
	MaxLineChars int

	// OffloadChars is the size, counted in characters (runes), above which
	// the output of other middleware's tools is saved to the backend and
	// replaced by a head and a pointer; 0 means DefaultOffloadChars. Negative is an error.
	OffloadChars int

	// ExecTimeout is the timeout execute uses when the model gives none and
	// MaxExecTimeout is the most it may ask for; 0 means DefaultExecTimeout
	// and DefaultMaxExecTimeout. Negative is an error.
	ExecTimeout, MaxExecTimeout time.Duration

	// Execute registers the execute tool even when Backend is nil. Tools are
	// collected once, before any per-thread backend exists, so with a nil
	// Backend the middleware cannot tell whether the agent's backend can run
	// commands. Setting Execute is the caller's promise that it can; a call
	// on a backend that cannot returns an error result. With a non-nil
	// Backend, execute is registered exactly when it implements
	// backend.Executor.
	Execute bool

	// Background offers run_in_background on execute, which hands the
	// command to tool.Call.Background (set by the tasks middleware) instead
	// of waiting for it. False leaves the parameter out of the schema.
	Background bool
}

// Name is the name of the filesystem middleware.
const Name = "filesystem"

// Middleware is the filesystem middleware. It is immutable after New, so it
// is safe to share between concurrent runs.
type Middleware struct {
	cfg      Config
	prompt   string
	excluded []string // known tools left out by the allowlist
}

// New returns the filesystem middleware. It returns an error for an invalid
// Config: an allowlist that omits read_file or names an unknown tool, a
// description for an unknown tool, or a negative limit.
func New(cfg Config) (*Middleware, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	m := &Middleware{cfg: withDefaults(cfg), prompt: DefaultPrompt}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	if cfg.Tools != nil {
		for _, name := range allTools {
			if !slices.Contains(cfg.Tools, name) {
				m.excluded = append(m.excluded, name)
			}
		}
	}
	return m, nil
}

func withDefaults(cfg Config) Config {
	if cfg.ReadLimit == 0 {
		cfg.ReadLimit = DefaultReadLimit
	}
	if cfg.MaxLineChars == 0 {
		cfg.MaxLineChars = DefaultMaxLineChars
	}
	if cfg.OffloadChars == 0 {
		cfg.OffloadChars = DefaultOffloadChars
	}
	if cfg.ExecTimeout == 0 {
		cfg.ExecTimeout = DefaultExecTimeout
	}
	if cfg.MaxExecTimeout == 0 {
		cfg.MaxExecTimeout = DefaultMaxExecTimeout
	}
	return cfg
}

func validate(cfg Config) error {
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"ReadLimit", int64(cfg.ReadLimit)},
		{"MaxLineChars", int64(cfg.MaxLineChars)},
		{"OffloadChars", int64(cfg.OffloadChars)},
		{"ExecTimeout", int64(cfg.ExecTimeout)},
		{"MaxExecTimeout", int64(cfg.MaxExecTimeout)},
	} {
		if f.v < 0 {
			return fmt.Errorf("filesystem: %s must not be negative", f.name)
		}
	}
	if cfg.Tools != nil {
		if !slices.Contains(cfg.Tools, ToolReadFile) {
			return fmt.Errorf("filesystem: Tools must include %s", ToolReadFile)
		}
		for _, name := range cfg.Tools {
			if !slices.Contains(allTools, name) {
				return fmt.Errorf("filesystem: unknown tool %q in Tools", name)
			}
		}
	}
	for name := range cfg.Descriptions {
		if !slices.Contains(allTools, name) {
			return fmt.Errorf("filesystem: Descriptions names unknown tool %q", name)
		}
	}
	return nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Tools implements agent.ToolProvider.
func (m *Middleware) Tools() []tool.Tool {
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
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if m.prompt != "" {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt)
	}
	if len(m.excluded) > 0 {
		req.Request.Tools = slices.DeleteFunc(slices.Clone(req.Request.Tools), func(s llm.ToolSchema) bool {
			return slices.Contains(m.excluded, s.Name)
		})
	}
	if b := m.requestBackend(req); b != nil {
		req.Request.Messages = m.offloadLargeMessages(ctx, b, req.Request.Messages)
	}
	return next(ctx, req)
}

// description returns the override for name, or the built-in text.
func (m *Middleware) description(name, builtin string) string {
	if d, ok := m.cfg.Descriptions[name]; ok {
		return d
	}
	return builtin
}

// requestBackend picks the configured backend, else the one the run resolved
// for the model request; nil when there is none.
func (m *Middleware) requestBackend(req *agent.ModelRequest) backend.Backend {
	if m.cfg.Backend != nil {
		return m.cfg.Backend
	}
	return req.Backend
}

// backendFor picks the configured backend, else the agent's.
func (m *Middleware) backendFor(c *tool.Call) (backend.Backend, error) {
	switch {
	case m.cfg.Backend != nil:
		return m.cfg.Backend, nil
	case c != nil && c.Backend != nil:
		return c.Backend, nil
	}
	return nil, errNoBackend
}
