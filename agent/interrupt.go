package agent

import (
	"encoding/json"
	"fmt"
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

// Interrupt pauses a run until a human decides on the listed actions. A tool
// or ToolWrapper returns *Interrupt as its error to request a pause.
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
