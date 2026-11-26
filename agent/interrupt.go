package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Errors Resume reports, through Final.Err, when its request cannot be
// carried out.
var (
	// ErrNoPendingInterrupt: the State holds no interrupted run.
	ErrNoPendingInterrupt = errors.New("agent: no pending interrupt to resume")
	// ErrMissingDecision: a pending action got no decision.
	ErrMissingDecision = errors.New("agent: missing decision for a pending action")
	// ErrInvalidDecision: a decision's type is not in its request's Allowed.
	ErrInvalidDecision = errors.New("agent: decision type not allowed for this action")
	// ErrResumeWithoutVersion: a resume with Config.Checkpointer set and the
	// zero Input.Version names a thread the checkpointer already holds, so
	// its first save would conflict. The error also wraps
	// checkpoint.ErrConflict.
	ErrResumeWithoutVersion = errors.New("agent: resume of a saved thread without its Version")
)

// errResumeMessages reports a ResumeWith whose Input carries messages: a
// resumed run must first answer the interrupted turn, so it appends none.
var errResumeMessages = errors.New("agent: a resumed run takes no Input.Messages")

// DecisionType is how a human answers a pending action.
//
// It is an alias of tool.ResumeType, so a decision relayed to a tool needs no
// conversion.
type DecisionType = tool.ResumeType

// The ways to answer an ActionRequest.
const (
	// Approve lets the action run as requested.
	Approve DecisionType = tool.ResumeApprove
	// Edit runs the action with Decision.Input instead of the original input.
	Edit DecisionType = tool.ResumeEdit
	// Reject refuses the action; Decision.Message is shown to the model (or
	// handed to the tool, see Interrupt.ReinvokeOnReject).
	Reject DecisionType = tool.ResumeReject
	// Respond answers on the tool's behalf with Decision.Message.
	Respond DecisionType = tool.ResumeRespond
)

// ActionRequest is one action a paused run needs a decision for.
type ActionRequest struct {
	// ToolUseID is the tool_use id of the pending call; a Decision answers
	// it by this id.
	ToolUseID string `json:"tool_use_id"`
	// Tool is the name of the tool the call targets.
	Tool string `json:"tool"`
	// Input is the call's raw JSON arguments.
	Input json.RawMessage `json:"input,omitempty"`
	// Reason explains to the human why a decision is needed.
	Reason string `json:"reason,omitempty"`
	// Allowed lists the decision types the host may answer with.
	Allowed []DecisionType `json:"allowed,omitempty"`
}

// Interrupt pauses a run until a human decides on the listed actions.
//
// A run pauses when a tool call needs approval and no Approver is configured,
// or when a tool or ToolWrapper returns *Interrupt (possibly wrapped) as its
// error. A tool raising one pauses only its own call, so just the Reason and
// Allowed of its first request are used; Allowed defaults to approve, reject
// and respond. When the call is resumed, the tool is invoked again with
// tool.Call.Resume describing the decision.
//
// The run emits KindInterrupt carrying an Interrupt that lists every pending
// action of the turn, then ends with ReasonInterrupted.
type Interrupt struct {
	// Requests lists the pending actions, one per call awaiting a decision.
	Requests []ActionRequest `json:"requests"`
	// ReinvokeOnReject, set by a tool or ToolWrapper raising the interrupt,
	// asks for a Reject decision to re-invoke it with tool.Call.Resume of
	// type tool.ResumeReject and the decision's message, so it can handle
	// the rejection itself (a subagent's task call passes it on to the
	// subagent). False keeps the default: the agent answers a rejected call
	// with an error result and never re-invokes the tool. It is recorded
	// with the pending call, so it survives a checkpoint. Interrupts the
	// agent raises for permission asks never set it.
	ReinvokeOnReject bool `json:"reinvoke_on_reject,omitempty"`
}

// Error implements error.
func (i *Interrupt) Error() string {
	return fmt.Sprintf("agent: interrupted: %d action(s) awaiting a decision", len(i.Requests))
}

// Decision is a human's answer to one ActionRequest, matched by ToolUseID.
type Decision struct {
	// ToolUseID is the tool_use id of the ActionRequest this decision
	// answers.
	ToolUseID string `json:"tool_use_id"`
	// Type is the kind of decision; it must be one the request allows.
	Type DecisionType `json:"type"`
	// Input is the replacement input for Edit.
	Input json.RawMessage `json:"input,omitempty"`
	// Message is the rejection reason for Reject or the answer for Respond.
	Message string `json:"message,omitempty"`
}

// Resume continues a run that ended with ReasonInterrupted, from the State it
// left (Final.State, possibly restored from JSON). decisions must answer every
// pending action, each with a type its request allows. Otherwise, or when s
// holds no interrupt, the stream is a single KindFinal with
// ReasonInvalidResume and the error (ErrNoPendingInterrupt,
// ErrMissingDecision, ErrInvalidDecision or another one), and s is left
// untouched.
//
// The interrupted turn is finished first; a call whose result was recorded
// is never run again. The decided calls are carried out first, in call
// order, and none is held back by another call interrupting:
//   - Approve runs the call; a tool that raised the interrupt is invoked
//     again with tool.Call.Resume set.
//   - Edit runs the call with Decision.Input, which is validated against the
//     tool's schema first; invalid input becomes an error result.
//   - Reject answers the call with an error result carrying Decision.Message;
//     a tool whose interrupt set ReinvokeOnReject is invoked again instead,
//     with tool.Call.Resume carrying the rejection.
//   - Respond answers a permission ask with Decision.Message as a successful
//     result, and re-invokes a tool that raised the interrupt with the
//     message in tool.Call.Resume.
//
// Every decided call that runs (approve, edit, respond to a tool, or reject
// a tool whose interrupt set ReinvokeOnReject) is checked against the
// permission rules again, on the input it runs with: the decision stands in
// for an approval, but a deny rule still refuses the call with a "Permission
// denied" error result.
//
// Calls that never started run after that, normally, and may interrupt
// again; so may a re-invoked tool. Once every call has a result, the results
// are placed right after the turn's assistant message (before anything a
// BeforeAgent hook added) and the run goes on like one started by Run, with
// the same hooks, a fresh budget, and threadID selecting the backend.
//
// Resume is ResumeWith with an Input holding just threadID and s, so its
// Version is the zero one: with Config.Checkpointer set it can resume only a
// thread the checkpointer does not hold yet. A saved thread is refused
// before anything runs (see ResumeWith); resume it with Thread.Resume, or
// with ResumeWith and the Version s was loaded at.
func (a *Agent) Resume(ctx context.Context, threadID string, s *state.State, decisions []Decision) iter.Seq2[Event, error] {
	return a.ResumeWith(ctx, Input{ThreadID: threadID, State: s}, decisions)
}

// ResumeWith is Resume with the per-run settings of in: in.State is the
// interrupted conversation, and in.ThreadID, in.Version, in.Backend and
// in.AgentName apply as they do for Run. A resumed run appends no messages,
// so a non-empty in.Messages makes the request invalid, like a missing
// decision: the stream is a single KindFinal with ReasonInvalidResume.
//
// A persisted run (Config.Checkpointer set, a ThreadID, not Ephemeral)
// with the zero in.Version would save as a new thread. When the
// checkpointer already holds the thread, that save would conflict, so the
// stream is a single KindFinal with ReasonInvalidResume and an error
// wrapping ErrResumeWithoutVersion and checkpoint.ErrConflict: no hook,
// model call or tool runs, nothing is saved and in.State is left untouched.
// A Load that fails (other than with checkpoint.ErrNotFound) ends it the
// same way with ReasonCheckpointError.
func (a *Agent) ResumeWith(ctx context.Context, in Input, decisions []Decision) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if a.closed.Load() {
			yield(a.closedEvent(in), ErrClosed)
			return
		}
		reason := ReasonInvalidResume
		resume, err := a.newResumption(in, decisions)
		if err == nil {
			reason, err = a.checkResumeVersion(ctx, in)
		}
		if err != nil {
			final := &Final{Reason: reason, Err: err, State: in.State, Version: in.Version}
			yield(Event{Kind: KindFinal, Agent: a.name(in), Time: time.Now(), Final: final}, err)
			return
		}
		a.stream(ctx, in, resume)(yield)
	}
}

// checkResumeVersion refuses a persisted resume with the zero Version of a
// thread the checkpointer already holds: its first save would conflict, so
// it ends before anything runs, with ReasonInvalidResume and
// ErrResumeWithoutVersion. A failed Load ends it with ReasonCheckpointError.
func (a *Agent) checkResumeVersion(ctx context.Context, in Input) (TerminalReason, error) {
	store := a.storeFor(in)
	if store == nil || in.Version != "" {
		return "", nil
	}
	_, v, err := store.Load(ctx, in.ThreadID)
	switch {
	case errors.Is(err, checkpoint.ErrNotFound):
		return "", nil
	case err != nil:
		return ReasonCheckpointError, fmt.Errorf("agent: checkpoint: %w", err)
	}
	return ReasonInvalidResume, fmt.Errorf(
		"%w: thread %q is saved; resume it with Thread.Resume, or ResumeWith and "+
			"the Version its State was loaded at: %w",
		ErrResumeWithoutVersion, in.ThreadID, checkpoint.Conflict(in.ThreadID, "", v))
}

// newResumption validates a ResumeWith request.
func (a *Agent) newResumption(in Input, decisions []Decision) (*resumption, error) {
	if len(in.Messages) > 0 {
		return nil, errResumeMessages
	}
	return newResumption(in.State, decisions)
}
