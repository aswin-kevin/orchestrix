package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
)

// Tool is one action the model can invoke. Everything beyond these four
// methods is an optional capability interface (ReadOnlyer, ConcurrencySafer,
// PermissionChecker, Subjecter, ...), found with As so that a wrapper which
// implements Unwrap passes its inner tool's capabilities through.
type Tool interface {
	// Name is the identifier the model uses; see Build for the legal alphabet.
	Name() string
	// Description tells the model what the tool does and when to use it.
	Description() string
	// Schema is the JSON schema of the tool input.
	Schema() map[string]any
	// Call runs the tool. Problems the model can fix belong in the Result
	// (IsError); the Go error is for failures of the host itself.
	Call(ctx context.Context, in json.RawMessage, c *Call) (Result, error)
}

// ReadOnlyer is implemented by tools that can report a call as leaving the
// world unchanged. A tool without it is never read-only.
type ReadOnlyer interface {
	// ReadOnly reports whether this call leaves the world unchanged.
	ReadOnly(in json.RawMessage) bool
}

// ConcurrencySafer is implemented by tools that can report a call as safe
// to run alongside others. A tool without it always runs alone.
type ConcurrencySafer interface {
	// ConcurrencySafe reports whether this call may run alongside others.
	ConcurrencySafe(in json.RawMessage) bool
}

// PermissionChecker is implemented by tools with a permission verdict of their
// own, consulted after the rules (see permission.Evaluate). The zero Decision
// and Ask mean no opinion: the rules and mode decide, as for a tool without
// it. Only Allow and Deny are acted on (see permission.Evaluate steps 2 and
// 7); Deny always is, Allow is honoured unless the mode is dont-ask
// (permission.ModeDontAsk). Ask has no effect on the verdict; its Message,
// when set, becomes the approval question when no rule or mode decides the
// call.
type PermissionChecker interface {
	// CheckPermission is the tool's own verdict for a call with input in.
	CheckPermission(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision
}

// Subjecter is implemented by tools whose permission rules match on a string
// (for example a shell command) rather than on paths.
type Subjecter interface {
	Subject(in json.RawMessage) string
}

// ShellSubjecter is implemented by tools whose subject is a shell command
// line. Permission rules then match each command in the line rather than the
// line as plain text, so "git status && rm x" is not covered by a rule for
// git alone.
type ShellSubjecter interface {
	ShellSubject() bool
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

// Dispatcher is implemented by a tool that stands for another tool, such as
// one that calls a tool by name. The agent loop calls Dispatch after
// validating the dispatcher's own input and runs the named tool with the
// returned input instead: permission, ToolWrappers, events and interrupts
// all see that target, so dispatching never bypasses what applies to the
// target when the model calls it directly. The dispatcher's own permission
// is still evaluated first, on its own input. A returned error becomes the
// call's error result. A target that is itself a Dispatcher is refused, and
// the dispatcher's Call is not reached for a valid dispatch. A wrapper that
// unwraps to a Dispatcher (see As) is a Dispatcher too, so a wrapper that
// must intercept Call of a Dispatcher has to implement Dispatcher itself.
type Dispatcher interface {
	Dispatch(in json.RawMessage) (name string, input json.RawMessage, err error)
}

// Availability is implemented by a tool that a run may call only under a
// condition of the run's State, such as a tool locked until a skill unlocks
// it. The agent loop asks it when it resolves a call, whether the model
// names the tool directly or as a Dispatcher's target, before any
// permission check or approval prompt: a non-nil error refuses the call,
// and its text is the call's error result. A tool should check the same
// condition in its own Call, so the lock also holds where nothing asks
// Available first.
type Availability interface {
	Available(s *state.State) error
}

// Call carries the per-invocation environment handed to Tool.Call.
type Call struct {
	// ToolUseID is the tool_use id of the call being executed.
	ToolUseID string
	// AgentID is the name of the agent whose run made the call.
	AgentID string
	// ThreadID is the run's thread id; empty for a run without one.
	ThreadID string
	// WorkingDir is the agent's Config.WorkingDir, the directory the tool
	// treats as current.
	WorkingDir string
	// Backend is the backend resolved for this run; it may be nil.
	Backend backend.Backend
	// State is the run's conversation state.
	State *state.State
	// Profile is the agent's model profile.
	Profile llm.Profile
	// Emit receives progress messages; use Progress, which tolerates nil.
	Emit func(progress string)
	// MaxOutputChars is the cap, in bytes, the tool should apply to its
	// output (see Truncate); zero means the caller imposes none.
	MaxOutputChars int
	// Resume is set when the tool is re-invoked after an interrupt.
	Resume *ResumeValue
	// ReadVisible reports whether a path found while searching may be shown
	// to the model; nil means every path is visible. Permission rules only
	// see the root a search starts from, so tools that list or search many
	// paths (ls, glob, grep) must drop what this rejects. Use Visible, which
	// tolerates nil.
	ReadVisible func(path string) bool
	// Background starts commands that outlive the call; the jobs middleware
	// sets it. nil means background execution is unavailable.
	Background Background
}

// Background starts shell commands as background jobs of the run's thread.
type Background interface {
	// Start runs command on c's backend without waiting for it and returns
	// the job's id. A zero timeout means the implementation's default.
	Start(ctx context.Context, c *Call, command string, timeout time.Duration) (id string, err error)
}

// ResumeType is how the human answered an interrupt. The agent package's
// DecisionType is an alias of it.
type ResumeType string

// The values of ResumeValue.Type.
const (
	// ResumeApprove: run the call as requested.
	ResumeApprove ResumeType = "approve"
	// ResumeEdit: run the call with ResumeValue.Input instead.
	ResumeEdit ResumeType = "edit"
	// ResumeReject: the call was refused, with ResumeValue.Message as the
	// reason. The agent answers a rejected call itself without re-invoking
	// the tool, unless the tool's interrupt set ReinvokeOnReject (see the
	// agent package's Interrupt); only such a tool sees this type.
	ResumeReject ResumeType = "reject"
	// ResumeRespond: ResumeValue.Message is the human's answer.
	ResumeRespond ResumeType = "respond"
)

// ResumeValue is the answer to an interrupt a tool raised earlier.
type ResumeValue struct {
	// Type is one of ResumeApprove, ResumeEdit, ResumeReject or
	// ResumeRespond.
	Type ResumeType
	// Input is the replacement input, set for ResumeEdit.
	Input json.RawMessage
	// Message is the rejection reason for ResumeReject or the answer for
	// ResumeRespond.
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

// Visible reports whether path may appear in search results; see
// ReadVisible. It is true when the receiver or its ReadVisible is nil.
func (c *Call) Visible(path string) bool {
	if c == nil || c.ReadVisible == nil {
		return true
	}
	return c.ReadVisible(path)
}

// Result is what a tool returns to the model.
type Result struct {
	// Content is the blocks returned to the model, normally text.
	Content []llm.Block `json:"content"`
	// IsError reports that the call failed and Content describes the error.
	IsError bool `json:"is_error,omitempty"`
	// Extra holds additional messages to append to the conversation after the
	// tool result.
	Extra []llm.Message `json:"extra,omitempty"`
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
