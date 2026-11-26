package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// EventKind identifies what an Event carries.
type EventKind string

// The kinds of Event a run emits. KindFinal is always the last one.
const (
	// KindText is assistant text (Event.Text): a delta while the reply
	// streams, but the whole reply's text in one event under
	// Config.NonStreaming or when a ModelWrapper answered without calling
	// next.
	KindText EventKind = "text"
	// KindThinking is model reasoning (Event.Text), a delta or the whole
	// reply's reasoning in the same cases as KindText.
	KindThinking EventKind = "thinking"
	// KindToolUse announces an attempt to run a tool call (ToolID, ToolName,
	// Input). It is emitted once the call's fate is known: right before the
	// call starts, when it is answered without running, or when it pauses
	// for a decision. A call that never started is not announced, and every
	// announced call gets a KindToolResult unless the run ends interrupted
	// with it pending. A call that pauses is announced again when it is
	// resumed, then with the input it runs with.
	KindToolUse EventKind = "tool_use"
	// KindToolResult reports a finished tool call (ToolID, ToolName,
	// ToolResult).
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
	// KindCustom is an event a hook, wrapper, permission check or tool sent
	// with Emit (Data). The agent never sends one itself.
	KindCustom EventKind = "custom"
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
	// ReasonTokenLimit: the run's token usage reached Config.MaxTotalTokens.
	ReasonTokenLimit TerminalReason = "token_limit"
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
	// in this process, but the store may not hold it. A save that found the
	// thread changed by another writer wraps checkpoint.ErrConflict: reopen
	// the thread (Agent.Thread) to continue from what that writer saved.
	ReasonCheckpointError TerminalReason = "checkpoint_error"
	// ReasonBusy: a Thread was asked to run while one of its runs was still
	// in progress (Final.Err is ErrThreadBusy; Final.State is nil), or the
	// thread's lease is held by another run (Final.Err wraps
	// checkpoint.ErrLeaseHeld; Final.State is the input State). Nothing ran.
	ReasonBusy TerminalReason = "busy"
	// ReasonInvalidOutput: the final answer failed Config.OutputSchema after
	// Config.OutputRetries corrections (Final.Err wraps ErrInvalidOutput).
	// Final.Text is the last reply.
	ReasonInvalidOutput TerminalReason = "invalid_output"
	// ReasonRefusal: the model declined to answer (llm.StopRefusal).
	// Final.Text is its reply; tool calls in that reply did not run. Not a
	// success.
	ReasonRefusal TerminalReason = "refusal"
	// ReasonInternalError: code the run calls on its own goroutine (the
	// provider, a ModelWrapper, the Checkpointer or its lease) panicked.
	// Final.Err is a *PanicError, or wraps ErrClosed for a run started
	// after Agent.Close. After a panic the end-of-run save is still
	// attempted, the lease released and the RunWrappers' end functions
	// called; Final.Version is the version of the last save that succeeded.
	// A run started after Close never ran: Final.State is the input State.
	ReasonInternalError TerminalReason = "internal_error"
)

// PanicError is the error of a run that ended with ReasonInternalError: it
// carries the value the run's code panicked with and the stack of the
// goroutine at the time.
type PanicError struct {
	// Value is what the code panicked with.
	Value any
	// Stack is the goroutine's stack trace at the time of the panic.
	Stack []byte
}

// Error implements error.
func (e *PanicError) Error() string {
	return fmt.Sprintf("agent: panic: %v", e.Value)
}

// Unwrap returns Value when it is an error, so errors.Is and errors.As see
// through the panic.
func (e *PanicError) Unwrap() error {
	err, _ := e.Value.(error)
	return err
}

// Event is one item of a run's output stream. Kind selects which of the other
// fields are set; see the EventKind constants.
type Event struct {
	// Kind says what the event carries.
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
	// Turn is the 1-based model turn the event belongs to: model output and
	// usage of the turn, and the events of the tool calls it requested.
	// Events emitted before the first model turn (such as the results of
	// calls answered by a resumed run) have Turn 0. KindFinal carries the
	// number of the last turn, Final.Turns. Events of a model call that
	// failed carry the number of the turn it attempted, which can exceed
	// Final.Turns: that counts recorded replies. A resumed run numbers its
	// own turns from 1. KindFinal of a run that never started (ReasonBusy,
	// ReasonInvalidResume, or a run after Agent.Close) has Turn 0.
	Turn int
	// Time is when the run emitted the event. An event forwarded from a
	// nested run keeps the time it had there.
	Time time.Time
	// RunID identifies the run that produced the event (RunInfo.RunID). An
	// event forwarded from a nested run keeps the nested run's id. Empty on
	// the KindFinal of a run that never started (ReasonBusy from Thread,
	// ReasonInvalidResume, or a run after Agent.Close).
	RunID string
	// Text is the delta or message of a KindText, KindThinking or
	// KindProgress event.
	Text string
	// ToolID is the tool_use id of the call, set on KindToolUse and
	// KindToolResult events.
	ToolID string
	// ToolName is the name of the tool, set on KindToolUse and
	// KindToolResult events.
	ToolName string
	// Input is the tool call's raw JSON arguments, set on KindToolUse.
	Input json.RawMessage
	// ToolResult is set for KindToolResult.
	ToolResult *tool.Result
	// Usage is set for KindUsage.
	Usage *llm.Usage
	// Interrupt is set for KindInterrupt.
	Interrupt *Interrupt
	// Final is set for KindFinal.
	Final *Final
	// Data is set on KindCustom: a value of the emitting package's own type,
	// so hosts can type-switch on it. KindCustom events are not checkpointed
	// or recorded in State.
	Data any
}

// Final is the outcome of a run, delivered with KindFinal.
type Final struct {
	// Reason says why the run ended.
	Reason TerminalReason
	// Text is the assistant's last text reply, empty if there was none.
	Text string
	// Usage is the token usage summed over the run's model calls.
	Usage llm.Usage
	// Turns is the number of model turns the run took.
	Turns int
	// Err is the underlying error for failure reasons, nil otherwise.
	Err error
	// State is the run's final state, from which a later run can continue.
	State *state.State
	// Version is the thread's checkpoint version after the run: the one
	// its last successful save returned, or Input.Version when it saved
	// nothing. Pass it as Input.Version to continue from State.
	Version checkpoint.Version
	// Output is the normalized compact JSON answer. It is non-nil exactly
	// when Config.OutputSchema applied to the run and Reason is
	// ReasonCompleted.
	Output json.RawMessage
}
