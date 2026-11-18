package hooks

import (
	"context"
	"encoding/json"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// ContextMetaKey is the Meta key of a user message injected by a hook's
// Context. Its value is the name of the event that produced it:
// "SessionStart" or "UserPromptSubmit". The message stays in State, so the
// conversation records what the hook added.
const ContextMetaKey = "orchestrix.hook"

// SubagentToolName is the tool name SubagentStart hooks fire for. It is
// agent.SubagentToolName, shared with the subagents package.
const SubagentToolName = agent.SubagentToolName

// Matcher selects the tools a hook applies to.
type Matcher struct {
	// Tools lists tool names. Empty, or containing "*", matches every tool.
	Tools []string
}

// ToolInput describes a tool call to a PreToolUse, PostToolUse or
// PostToolUseFailure hook.
type ToolInput struct {
	// ThreadID is the run's thread id; empty for a run without one.
	ThreadID string
	// Agent is the name of the agent executing the call.
	Agent string
	// Depth is 0 for a top-level run and the parent's Depth+1 inside a
	// subagent run (agent.RunInfo.Depth).
	Depth int
	// ParentToolID is the tool_use id of the call that started this nested
	// run; empty for a top-level run (agent.RunInfo.ParentToolID).
	ParentToolID string
	// ToolUseID is the id of the model's tool_use block.
	ToolUseID string
	// Tool is the tool name.
	Tool string
	// Input is the call's input as it stands: the model's, or the previous
	// hook's rewrite.
	Input json.RawMessage
	// State is the run's state; hooks may read and change it.
	State *state.State
}

// PreToolUseOutput is a PreToolUse hook's verdict. The zero value lets the
// call proceed unchanged.
type PreToolUseOutput struct {
	// Block stops the tool from running; the model gets an error result
	// "Blocked by hook: <Reason>".
	Block  bool
	Reason string
	// UpdatedInput, when non-empty, replaces the call's input. It is
	// validated against the tool's schema and checked by the permission
	// rules again: a Deny refuses the call, an Ask counts as allowed, since
	// hooks are trusted host code, and an Allow carrying its own input
	// rewrite (the tool's normalisation) replaces it. Nil keeps the input.
	UpdatedInput json.RawMessage
	// Context, when non-empty, is appended to the tool result as an extra
	// text block.
	Context string
}

// PostToolUseOutput is a PostToolUse or PostToolUseFailure hook's verdict.
// The zero value keeps the result.
type PostToolUseOutput struct {
	// Result, when non-nil, replaces the tool result; later hooks see the
	// replacement.
	Result *tool.Result
	// Context, when non-empty, is appended to the result as an extra text
	// block.
	Context string
}

// PromptInput describes the user input of a run to a UserPromptSubmit hook.
type PromptInput struct {
	// ThreadID is the run's thread id; empty for a run without one.
	ThreadID string
	// Depth is 0 for a top-level run and the parent's Depth+1 inside a
	// subagent run (agent.RunInfo.Depth).
	Depth int
	// ParentToolID is the tool_use id of the call that started this nested
	// run; empty for a top-level run (agent.RunInfo.ParentToolID).
	ParentToolID string
	// Messages are the messages this run's input appended to State.
	Messages []llm.Message
	// State is the run's state.
	State *state.State
}

// PromptOutput is a UserPromptSubmit hook's verdict. The zero value accepts
// the prompt.
type PromptOutput struct {
	// Block rejects the prompt: the run's input is removed from State and
	// the run ends with agent.ReasonHookStopped. Reason is in the error.
	Block  bool
	Reason string
	// Context, when non-empty, is added to the conversation as a user
	// message marked with ContextMetaKey.
	Context string
}

// SessionInput describes a run to SessionStart and SessionEnd hooks.
type SessionInput struct {
	// ThreadID is the run's thread id; empty for a run without one.
	ThreadID string
	// Resumed reports a run started by Resume or ResumeWith.
	Resumed bool
	// Depth is 0 for a top-level run and the parent's Depth+1 inside a
	// subagent run (agent.RunInfo.Depth).
	Depth int
	// ParentToolID is the tool_use id of the call that started this nested
	// run; empty for a top-level run (agent.RunInfo.ParentToolID).
	ParentToolID string
	// State is the run's state.
	State *state.State
}

// SessionOutput is a SessionStart hook's verdict. The zero value adds
// nothing.
type SessionOutput struct {
	// Context, when non-empty, is added to the conversation as a user
	// message marked with ContextMetaKey.
	Context string
}

// StopInput describes the reply that would complete the run to a Stop hook.
type StopInput struct {
	// ThreadID is the run's thread id; empty for a run without one.
	ThreadID string
	// Depth is 0 for a top-level run and the parent's Depth+1 inside a
	// subagent run (agent.RunInfo.Depth).
	Depth int
	// ParentToolID is the tool_use id of the call that started this nested
	// run; empty for a top-level run (agent.RunInfo.ParentToolID).
	ParentToolID string
	// Final is the text of the assistant's closing reply.
	Final string
	// State is the run's state.
	State *state.State
}

// StopOutput is a Stop hook's verdict. The zero value lets the run complete.
type StopOutput struct {
	// Continue, when non-empty, is appended to State as a user message and
	// the run takes another turn. The first non-empty Continue wins; the run
	// can be kept going at most agent.MaxStopContinuations times.
	Continue string
}

// SubagentInput describes a subagent launch to a SubagentStart hook.
type SubagentInput struct {
	// ThreadID is the run's thread id; empty for a run without one.
	ThreadID string
	// Depth is 0 for a top-level run and the parent's Depth+1 inside a
	// subagent run (agent.RunInfo.Depth).
	Depth int
	// ParentToolID is the tool_use id of the call that started this nested
	// run; empty for a top-level run (agent.RunInfo.ParentToolID).
	ParentToolID string
	// ToolUseID is the id of the tool_use block that launches the subagent.
	ToolUseID string
	// SubagentType is the input's subagent_type; empty when absent.
	SubagentType string
	// Description is the input's description, the task given to the
	// subagent; empty when absent.
	Description string
}

// SubagentOutput is a SubagentStart hook's verdict. The zero value lets the
// launch proceed.
type SubagentOutput struct {
	// Block refuses the launch; the model gets an error result
	// "Blocked by hook: <Reason>".
	Block  bool
	Reason string
	// Context, when non-empty, is appended to the task description as
	// "\n\n" + Context.
	Context string
}

// PreToolUseHook runs before a matching tool call.
//
// A call that a human decision resumes goes through the tool wrapper chain
// again, so the hook fires again for it: once for the attempt that
// interrupted and once for the resumed one.
type PreToolUseHook struct {
	Matcher
	// Fn decides the call's fate. A returned error is not a verdict: it
	// fails the call, or stops the run when it wraps agent.ErrStopRun.
	Fn func(context.Context, ToolInput) (PreToolUseOutput, error)
}

// PostToolUseHook runs after a matching tool call returned a result.
type PostToolUseHook struct {
	Matcher
	// Fn sees the result as it stands, after earlier hooks. A returned
	// error fails the call, or stops the run when it wraps agent.ErrStopRun.
	Fn func(context.Context, ToolInput, tool.Result) (PostToolUseOutput, error)
}

// Config lists the hooks of each lifecycle point; a nil list has none. Hooks
// of one point run in order. The tool hooks (PreToolUse, PostToolUse,
// PostToolUseFailure, SubagentStart) may run concurrently for the
// concurrency-safe calls of one turn, so a hook that touches shared data must
// synchronize. A hook returning an error wrapping
// agent.ErrStopRun ends the run with agent.ReasonHookStopped; any other
// error is a middleware error, or inside a tool call the call's failure.
//
// Hooks fire in every run of an agent that has the middleware, including
// subagent runs (a subagent inherits its parent's middleware unless its spec
// replaces it) and resumed runs; the Depth and ParentToolID of each input
// tell a nested run from a top-level one, and SessionInput.Resumed a resumed
// one.
type Config struct {
	// PreToolUse runs before a tool call, after the permission rules
	// allowed it. Each hook sees the previous one's input rewrite.
	//
	// When a human decision resumes a call (an interrupt raised by a tool
	// or a wrapper), the wrapper chain runs again and so do these hooks
	// and SubagentStart for that call.
	PreToolUse []PreToolUseHook
	// PostToolUse runs after a call that succeeded.
	PostToolUse []PostToolUseHook
	// PostToolUseFailure runs after a call whose result is an error.
	PostToolUseFailure []PostToolUseHook
	// UserPromptSubmit runs at the start of a run that has input messages.
	UserPromptSubmit []func(context.Context, PromptInput) (PromptOutput, error)
	// SessionStart runs at the start of every run, resumed or not: each
	// Resume and each turn of a Thread runs it again, and the Context it
	// returns is appended to State every time, so it accumulates over a
	// conversation. A hook that should inject only once must track that
	// itself (for example in State).
	SessionStart []func(context.Context, SessionInput) (SessionOutput, error)
	// SessionEnd runs when a run ends, whatever the reason, unless the
	// consumer stopped reading the run's events before the end (broke out of
	// the range loop): then the run stops without running it.
	SessionEnd []func(context.Context, SessionInput) error
	// Stop runs when the model's reply would complete the run.
	Stop []func(context.Context, StopInput) (StopOutput, error)
	// SubagentStart runs before the SubagentToolName tool is called, and
	// again for a call resumed after an interrupt (see PreToolUse).
	SubagentStart []func(context.Context, SubagentInput) (SubagentOutput, error)
}
