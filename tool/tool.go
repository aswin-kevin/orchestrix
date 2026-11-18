// Package tool defines the contract between the agent loop and the actions a
// model can take: the Tool interface, its optional extensions, typed tool
// construction with generated JSON schemas, input validation, a registry and
// output truncation.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
)

// Tool is one action the model can invoke.
type Tool interface {
	// Name is the identifier the model uses; see Build for the legal alphabet.
	Name() string
	// Description tells the model what the tool does and when to use it.
	Description() string
	// Schema is the JSON schema of the tool input.
	Schema() map[string]any
	// ReadOnly reports whether this call leaves the world unchanged.
	ReadOnly(in json.RawMessage) bool
	// ConcurrencySafe reports whether this call may run alongside others.
	ConcurrencySafe(in json.RawMessage) bool
	// CheckPermission is the tool's own verdict, consulted after the rules.
	CheckPermission(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision
	// Call runs the tool. Problems the model can fix belong in the Result
	// (IsError); the Go error is for failures of the host itself.
	Call(ctx context.Context, in json.RawMessage, c *Call) (Result, error)
}

// Subjecter is implemented by tools whose permission rules match on a string
// (for example a shell command) rather than on paths.
type Subjecter interface {
	Subject(in json.RawMessage) string
}

// PathAccessor is implemented by tools that touch the file system. Every path
// the call touches must be reported as a virtual absolute path.
type PathAccessor interface {
	Paths(in json.RawMessage) []permission.PathAccess
}

// Editor is implemented by tools that modify files, so permission modes such
// as acceptEdits can treat them as a group.
type Editor interface {
	IsEdit(in json.RawMessage) bool
}

// RawInputer is implemented by tools that want their input passed through
// without schema validation.
type RawInputer interface {
	AcceptsRawInput() bool
}

// Call carries the per-invocation environment handed to Tool.Call.
type Call struct {
	ToolUseID  string
	AgentID    string
	ThreadID   string
	WorkingDir string
	Backend    backend.Backend
	State      *state.State
	Profile    llm.Profile
	// Emit receives progress messages; use Progress, which tolerates nil.
	Emit func(progress string)
	// MaxOutputChars is the cap the tool should apply to its output; zero
	// means the caller imposes none.
	MaxOutputChars int
	// Resume is set when the tool is re-invoked after an interrupt.
	Resume *ResumeValue
}

// ResumeValue is the answer to an interrupt a tool raised earlier.
type ResumeValue struct {
	Type    string
	Input   json.RawMessage
	Message string
}

// Progress reports a progress message. It does nothing when the receiver or
// its Emit callback is nil, so tools can call it unconditionally.
func (c *Call) Progress(msg string) {
	if c == nil || c.Emit == nil {
		return
	}
	c.Emit(msg)
}

// Result is what a tool returns to the model.
type Result struct {
	Content []llm.Block
	IsError bool
	// Extra holds additional messages to append to the conversation after the
	// tool result.
	Extra []llm.Message
}

// Text returns a successful single-text result.
func Text(s string) Result {
	return Result{Content: []llm.Block{llm.TextBlock(s)}}
}

// Errorf returns an error result the model can read and react to.
func Errorf(format string, a ...any) Result {
	r := Text(fmt.Sprintf(format, a...))
	r.IsError = true
	return r
}

// Flatten joins the text blocks of the result with newlines; non-text blocks
// are skipped.
func (r Result) Flatten() string {
	var parts []string
	for _, b := range r.Content {
		if b.Type == llm.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
