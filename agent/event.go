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
	// KindToolUse announces an attempt to run a tool call (ToolID, ToolName,
	// Input). It is emitted once the call's fate is known: right before the
	// call starts, when it is answered without running, or when it pauses
	// for a decision. A call that never started is not announced, and every
	// announced call gets a KindToolResult unless the run ends interrupted
	// with it pending. A call that pauses is announced again when it is
	// resumed, then with the input it runs with.
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
	// ReasonInterrupted: a tool call awaits a human decision; see
	// Agent.Resume.
	ReasonInterrupted TerminalReason = "interrupted"
	// ReasonInvalidResume: Agent.Resume was called without a pending
	// interrupt or with decisions that do not answer it (Final.Err says
	// which). Nothing ran and State is unchanged. It is also the reason when
	// a BeforeAgent hook of the resumed run removed the interrupted turn's
	// assistant message; the hooks have run then, but no tool call has.
	ReasonInvalidResume TerminalReason = "invalid_resume"
	// ReasonModelError: the model call failed and could not be recovered.
	ReasonModelError TerminalReason = "model_error"
	// ReasonPromptTooLong: the conversation no longer fits the context window
	// and could not be reduced.
	ReasonPromptTooLong TerminalReason = "prompt_too_long"
	// ReasonAbortedStreaming: the context was cancelled during a model call.
	ReasonAbortedStreaming TerminalReason = "aborted_streaming"
	// ReasonAbortedTools: the context was cancelled during tool execution.
	ReasonAbortedTools TerminalReason = "aborted_tools"
	// ReasonHookStopped: a hook or a ToolWrapper ended the run on purpose by
	// returning an error wrapping ErrStopRun (Final.Err).
	ReasonHookStopped TerminalReason = "hook_stopped"
	// ReasonMiddlewareError: a middleware hook returned an error.
	ReasonMiddlewareError TerminalReason = "middleware_error"
	// ReasonCheckpointError: saving the run's State to Config.Checkpointer,
	// or capturing or restoring its workspace (Config.SyncWorkspace),
	// failed (Final.Err wraps the error). A failure at the end of the run
	// is joined into Final.Err and sets this reason only when the run had
	// otherwise succeeded (Final.Err nil). That includes an interrupted run:
	// its pending record is still in Final.State, so Pending and Resume work
	// in this process, but the store may not hold it.
	ReasonCheckpointError TerminalReason = "checkpoint_error"
	// ReasonBusy: a Thread was asked to run while one of its runs was still
	// in progress (Final.Err is ErrThreadBusy). Nothing ran and Final.State
	// is nil.
	ReasonBusy TerminalReason = "busy"
	// ReasonInvalidOutput: the final answer failed Config.OutputSchema after
	// OutputRetries corrections (Final.Err wraps ErrInvalidOutput). Final.Text
	// is the last reply.
	ReasonInvalidOutput TerminalReason = "invalid_output"
)

// Event is one item of a run's output stream. Kind selects which of the other
// fields are set; see the EventKind constants.
type Event struct {
	Kind EventKind
	// Agent is the name of the agent that produced the event: its run's
	// Input.AgentName or Config.Name. An event forwarded from a nested run
	// (see Forward) keeps the nested agent's name.
	Agent string
	// ParentToolID is set on an event forwarded from a nested run (see
	// Forward): the tool_use id of the call under which the run that
	// produced the event was started. An event forwarded through several
	// levels keeps the id of the first level. Empty for a run's own events.
	ParentToolID string
	Text         string
	ToolID       string
	ToolName     string
	Input        json.RawMessage
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
	// Output is the normalized compact JSON answer. It is non-nil exactly
	// when Config.OutputSchema applied to the run and Reason is
	// ReasonCompleted.
	Output json.RawMessage
}
