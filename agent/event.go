package agent

import (
	"encoding/json"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// EventKind identifies what an Event carries.
type EventKind string

// The kinds of Event a run emits. KindFinal is always the last one.
const (
	// KindText is a delta of assistant text (Event.Text).
	KindText EventKind = "text"
	// KindThinking is a delta of model reasoning (Event.Text).
	KindThinking EventKind = "thinking"
	// KindToolUse announces a tool call (ToolID, ToolName, Input).
	KindToolUse EventKind = "tool_use"
	// KindToolResult reports a finished tool call (ToolID, ToolName, ToolResult).
	KindToolResult EventKind = "tool_result"
	// KindProgress is a progress message from a running tool (Text).
	KindProgress EventKind = "progress"
	// KindUsage reports token usage for a model call (Usage).
	KindUsage EventKind = "usage"
	// KindRetry tells the host a model call is being retried, so it can
	// discard any partial output streamed so far.
	KindRetry EventKind = "retry"
	// KindInterrupt reports that the run is waiting for a human decision
	// (Interrupt).
	KindInterrupt EventKind = "interrupt"
	// KindFinal ends the stream (Final).
	KindFinal EventKind = "final"
)

// TerminalReason says why a run ended.
type TerminalReason string

// The reasons a run can end.
const (
	// ReasonCompleted: the model finished without requesting more tools.
	ReasonCompleted TerminalReason = "completed"
	// ReasonMaxTurns: Config.MaxTurns was reached.
	ReasonMaxTurns TerminalReason = "max_turns"
	// ReasonTimeout: Config.MaxDuration elapsed.
	ReasonTimeout TerminalReason = "timeout"
	// ReasonInterrupted: a tool or middleware asked for human input.
	ReasonInterrupted TerminalReason = "interrupted"
	// ReasonModelError: the model call failed and could not be recovered.
	ReasonModelError TerminalReason = "model_error"
	// ReasonPromptTooLong: the conversation no longer fits the context window
	// and could not be reduced.
	ReasonPromptTooLong TerminalReason = "prompt_too_long"
	// ReasonAbortedStreaming: the context was cancelled during a model call.
	ReasonAbortedStreaming TerminalReason = "aborted_streaming"
	// ReasonAbortedTools: the context was cancelled during tool execution.
	ReasonAbortedTools TerminalReason = "aborted_tools"
	// ReasonHookStopped: a hook ended the run on purpose.
	ReasonHookStopped TerminalReason = "hook_stopped"
	// ReasonMiddlewareError: a middleware hook returned an error.
	ReasonMiddlewareError TerminalReason = "middleware_error"
)

// Event is one item of a run's output stream. Kind selects which of the other
// fields are set; see the EventKind constants.
type Event struct {
	Kind EventKind
	// Agent is the Config.Name of the agent that produced the event.
	Agent    string
	Text     string
	ToolID   string
	ToolName string
	Input    json.RawMessage
	// ToolResult is set for KindToolResult.
	ToolResult *tool.Result
	// Usage is set for KindUsage.
	Usage *llm.Usage
	// Interrupt is set for KindInterrupt.
	Interrupt *Interrupt
	// Final is set for KindFinal.
	Final *Final
}

// Final is the outcome of a run, delivered with KindFinal.
type Final struct {
	Reason TerminalReason
	// Text is the assistant's last text reply, empty if there was none.
	Text  string
	Usage llm.Usage
	// Turns is the number of model turns the run took.
	Turns int
	// Err is the underlying error for failure reasons, nil otherwise.
	Err error
	// State is the run's final state, from which a later run can continue.
	State *state.State
}
