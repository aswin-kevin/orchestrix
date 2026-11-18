package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"

	"github.com/aswin-kevin/orchestrix/state"
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
)

// DecisionType is how a human answers a pending action.
type DecisionType string

// The ways to answer an ActionRequest.
const (
	// Approve lets the action run as requested.
	Approve DecisionType = "approve"
	// Edit runs the action with Decision.Input instead of the original input.
	Edit DecisionType = "edit"
	// Reject refuses the action; Decision.Message is shown to the model.
	Reject DecisionType = "reject"
	// Respond answers on the tool's behalf with Decision.Message.
	Respond DecisionType = "respond"
)

// ActionRequest is one action a paused run needs a decision for.
type ActionRequest struct {
	ToolUseID string
	Tool      string
	Input     json.RawMessage
	// Reason explains to the human why a decision is needed.
	Reason string
	// Allowed lists the decision types the host may answer with.
	Allowed []DecisionType
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
	Requests []ActionRequest
}

// Error implements error.
func (i *Interrupt) Error() string {
	return fmt.Sprintf("agent: interrupted: %d action(s) awaiting a decision", len(i.Requests))
}

// Decision is a human's answer to one ActionRequest, matched by ToolUseID.
type Decision struct {
	ToolUseID string
	Type      DecisionType
	// Input is the replacement input for Edit.
	Input json.RawMessage
	// Message is the rejection reason for Reject or the answer for Respond.
	Message string
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
// is never run again:
//   - Approve runs the call; a tool that raised the interrupt is invoked
//     again with tool.Call.Resume set.
//   - Edit runs the call with Decision.Input, which is validated against the
//     tool's schema first; invalid input becomes an error result.
//   - Reject answers the call with an error result carrying Decision.Message.
//   - Respond answers a permission ask with Decision.Message as a successful
//     result, and re-invokes a tool that raised the interrupt with the
//     message in tool.Call.Resume.
//
// Calls that never started run normally and may interrupt again. Then the
// run goes on like one started by Run, with the same hooks, a fresh budget,
// and threadID selecting the backend.
func (a *Agent) Resume(ctx context.Context, threadID string, s *state.State, decisions []Decision) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		resume, err := newResumption(s, decisions)
		if err != nil {
			final := &Final{Reason: ReasonInvalidResume, Err: err, State: s}
			yield(Event{Kind: KindFinal, Agent: a.cfg.Name, Final: final}, err)
			return
		}
		a.stream(ctx, Input{ThreadID: threadID, State: s}, resume)(yield)
	}
}
