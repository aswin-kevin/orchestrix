package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// rejectedToolResult answers a call the human rejected; the rejection
// message, if any, follows it after a colon.
const rejectedToolResult = "User rejected this action"

// interruptOrigin says what paused a call.
type interruptOrigin string

const (
	// originPermission: the call needed an approval and no Approver was
	// configured to give it.
	originPermission interruptOrigin = "permission"
	// originTool: the tool, or a ToolWrapper around it, returned *Interrupt.
	originTool interruptOrigin = "tool"
)

// permissionDecisions are the answers a permission ask accepts.
var permissionDecisions = []DecisionType{Approve, Edit, Reject, Respond}

// defaultToolDecisions are the answers a tool interrupt accepts when the tool
// lists none.
var defaultToolDecisions = []DecisionType{Approve, Reject, Respond}

// pendingKey holds the record of an interrupted turn until Resume finishes it
// or a new Run abandons it. It is private: a subagent never inherits it.
var pendingKey = state.NewKey[pendingTurn]("agent.pending", state.Private[pendingTurn]())

// errTurnMissing reports a State whose history no longer holds the assistant
// message of the interrupted turn, so its results have nowhere to go.
var errTurnMissing = errors.New("agent: resume: the interrupted turn is no longer in the conversation")

// pendingTurn records how far an interrupted turn got. The assistant message
// is in history; the user message with its tool results is not, because it
// is appended only once every call has a result.
//
// The assistant message is found again by its tool_use ids rather than by
// position, because hooks may add, drop or compact messages in between.
type pendingTurn struct {
	// Calls are all the calls of the turn, in call order.
	Calls []callRecord `json:"calls"`
}

// callRecord is one call of an interrupted turn. A call with a Result is
// done and never runs again; a call with an Interrupt awaits a decision; a
// call with neither was not started and is scheduled afresh on resume.
type callRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Input is kept as a string because a call that never started may carry
	// input that is not valid JSON.
	Input     string         `json:"input"`
	Result    *tool.Result   `json:"result,omitempty"`
	Interrupt *callInterrupt `json:"interrupt,omitempty"`
}

// callInterrupt is the decision a pending call waits for.
type callInterrupt struct {
	Origin  interruptOrigin `json:"origin"`
	Request ActionRequest   `json:"request"`
}

// resumedCall is the human's answer to the interrupt a call raised earlier.
type resumedCall struct {
	origin   interruptOrigin
	decision Decision
}

// resumption is a validated request to finish an interrupted turn.
type resumption struct {
	turn pendingTurn
	// decisions holds the decision for every pending call, by tool_use id.
	decisions map[string]Decision
}

// askInterrupt makes c pending on a permission ask no Approver can answer.
func askInterrupt(c *toolCall, reason string) *callInterrupt {
	return &callInterrupt{
		Origin: originPermission,
		Request: ActionRequest{
			ToolUseID: c.id,
			Tool:      c.name,
			Input:     c.input,
			Reason:    reason,
			Allowed:   slices.Clone(permissionDecisions),
		},
	}
}

// toolInterrupt makes c pending on the *Interrupt its tool chain returned.
// The interrupt is about this one call, so only the reason and the allowed
// decisions of its first request are used.
func toolInterrupt(c *toolCall, intr *Interrupt) *callInterrupt {
	req := ActionRequest{ToolUseID: c.id, Tool: c.name, Input: c.input}
	if len(intr.Requests) > 0 {
		req.Reason = intr.Requests[0].Reason
		req.Allowed = slices.Clone(intr.Requests[0].Allowed)
	}
	if len(req.Allowed) == 0 {
		req.Allowed = slices.Clone(defaultToolDecisions)
	}
	return &callInterrupt{Origin: originTool, Request: req}
}

// interrupt records the turn's progress in State and saves it, reports the
// pending requests and ends the run waiting for decisions.
func (r *run) interrupt(calls []*toolCall) *Final {
	turn := pendingTurn{Calls: make([]callRecord, len(calls))}
	for i, c := range calls {
		turn.Calls[i] = callRecord{
			ID:        c.id,
			Name:      c.name,
			Input:     string(c.input),
			Result:    c.result,
			Interrupt: c.interrupt,
		}
	}
	pendingKey.Set(r.state, turn)
	if err := r.checkpoint(true); err != nil {
		return r.checkpointFailure(err)
	}
	r.emit(Event{Kind: KindInterrupt, Interrupt: &Interrupt{Requests: turn.requests()}})
	return r.finish(ReasonInterrupted, nil)
}

// Pending returns the pending actions of the interrupted run recorded in s,
// in call order: the Interrupt that run reported with KindInterrupt. s is
// the Final.State of a run that ended with ReasonInterrupted, possibly
// restored from JSON. Pending returns false when s is nil or holds no
// interrupted run (it was never interrupted, or was resumed or run again
// since). The result is a copy: changing it never touches s.
func Pending(s *state.State) (*Interrupt, bool) {
	if s == nil {
		return nil, false
	}
	turn, ok := pendingKey.Get(s)
	if !ok {
		return nil, false
	}
	requests := turn.requests()
	if len(requests) == 0 {
		return nil, false
	}
	return &Interrupt{Requests: requests}, true
}

// requests returns a copy of the requests of the turn's pending calls, in
// call order.
func (t pendingTurn) requests() []ActionRequest {
	var requests []ActionRequest
	for _, rec := range t.Calls {
		if rec.Interrupt == nil {
			continue
		}
		req := rec.Interrupt.Request
		req.Input = bytes.Clone(req.Input)
		req.Allowed = slices.Clone(req.Allowed)
		requests = append(requests, req)
	}
	return requests
}

// resumeTurn finishes the interrupted turn. It runs after the BeforeAgent
// hooks, so it locates the turn's assistant message afresh in case they
// changed the history. It does not count toward MaxTurns: the model call that
// produced the turn belonged to the run that was interrupted.
func (r *run) resumeTurn() *Final {
	assistantIndex, ok := r.resume.turn.assistantIndex(r.state.Messages())
	if !ok {
		return r.finish(ReasonInvalidResume, errTurnMissing)
	}
	return r.toolTurn(assistantIndex, r.restoreCalls())
}

// restoreCalls rebuilds the calls of the interrupted turn. Recorded results
// are kept as they are; every other call is prepared afresh, and a pending
// one gets its decision applied, so that its KindToolUse event already shows
// the input it runs with.
func (r *run) restoreCalls() []*toolCall {
	calls := make([]*toolCall, len(r.resume.turn.Calls))
	for i, rec := range r.resume.turn.Calls {
		if rec.Result != nil {
			calls[i] = &toolCall{id: rec.ID, name: rec.Name, result: rec.Result}
			continue
		}
		c := r.prepare(llm.Block{Type: llm.BlockToolUse, ID: rec.ID, Name: rec.Name, Input: json.RawMessage(rec.Input)})
		if rec.Interrupt != nil && c.blocked == nil {
			c.resumed = &resumedCall{origin: rec.Interrupt.Origin, decision: r.resume.decisions[rec.ID]}
			c.blocked = c.applyDecision()
		}
		calls[i] = c
	}
	return calls
}

// applyDecision prepares a resumed call to continue as the human decided. It
// returns the result of a decision that answers the call without running it
// (a rejection, a response to a permission ask, or an edit with invalid
// input), or nil when the call runs.
func (c *toolCall) applyDecision() *tool.Result {
	if res := c.answerWithoutRunning(); res != nil {
		return res
	}
	d := c.resumed.decision
	if d.Type == Edit {
		c.input = d.Input
	}
	if c.resumed.origin == originTool {
		c.resume = &tool.ResumeValue{Type: string(d.Type), Input: d.Input, Message: d.Message}
	}
	return nil
}

// answerWithoutRunning returns the result of a decision that settles a
// resumed call without running it, or nil for a decision that runs it.
func (c *toolCall) answerWithoutRunning() *tool.Result {
	d := c.resumed.decision
	switch {
	case d.Type == Reject:
		if d.Message == "" {
			return errorResult(rejectedToolResult)
		}
		return errorResult("%s: %s", rejectedToolResult, d.Message)
	case d.Type == Respond && c.resumed.origin == originPermission:
		res := tool.Text(d.Message)
		return &res
	case d.Type == Edit:
		if err := checkEditedInput(c.tool, d.Input); err != nil {
			return errorResult("Edited input is invalid: %v", err)
		}
	}
	return nil
}

// checkEditedInput checks input a human supplied in place of the model's.
// Unlike model input it gets no lenient parsing: it must be valid JSON.
func checkEditedInput(t tool.Tool, in json.RawMessage) error {
	if !json.Valid(in) {
		return errors.New("not valid JSON")
	}
	return checkInput(t, in)
}

// newResumption validates a Resume request against the pending record in s.
func newResumption(s *state.State, decisions []Decision) (*resumption, error) {
	if s == nil {
		return nil, ErrNoPendingInterrupt
	}
	turn, ok := pendingKey.Get(s)
	if !ok {
		return nil, ErrNoPendingInterrupt
	}
	if _, ok := turn.assistantIndex(s.Messages()); !ok {
		return nil, errTurnMissing
	}
	byID, err := turn.matchDecisions(decisions)
	if err != nil {
		return nil, err
	}
	return &resumption{turn: turn, decisions: byID}, nil
}

// assistantIndex returns the position in msgs of the turn's assistant
// message: the last assistant message whose tool_use blocks include every
// call of the turn.
func (t pendingTurn) assistantIndex(msgs []llm.Message) (int, bool) {
	for i, m := range slices.Backward(msgs) {
		if m.Role == llm.RoleAssistant && t.madeIn(m) {
			return i, true
		}
	}
	return 0, false
}

// madeIn reports whether m carries a tool_use for every call of the turn.
func (t pendingTurn) madeIn(m llm.Message) bool {
	ids := make(map[string]bool)
	for _, use := range m.ToolUses() {
		ids[use.ID] = true
	}
	return !slices.ContainsFunc(t.Calls, func(rec callRecord) bool { return !ids[rec.ID] })
}

// matchDecisions pairs decisions with the pending calls. Every pending call
// needs exactly one decision of a type its request allows, and every
// decision must answer a pending call.
func (t pendingTurn) matchDecisions(decisions []Decision) (map[string]Decision, error) {
	requests := make(map[string]ActionRequest)
	for _, rec := range t.Calls {
		if rec.Interrupt != nil {
			requests[rec.ID] = rec.Interrupt.Request
		}
	}
	byID := make(map[string]Decision, len(decisions))
	for _, d := range decisions {
		req, pending := requests[d.ToolUseID]
		_, duplicate := byID[d.ToolUseID]
		switch {
		case !pending:
			return nil, fmt.Errorf("agent: resume: no pending action for tool call %q", d.ToolUseID)
		case duplicate:
			return nil, fmt.Errorf("agent: resume: more than one decision for tool call %q", d.ToolUseID)
		case !slices.Contains(req.Allowed, d.Type):
			return nil, fmt.Errorf("%w: %q for tool call %q (allowed: %v)", ErrInvalidDecision, d.Type, d.ToolUseID, req.Allowed)
		}
		byID[d.ToolUseID] = d
	}
	for _, rec := range t.Calls {
		if _, decided := byID[rec.ID]; rec.Interrupt != nil && !decided {
			return nil, fmt.Errorf("%w: tool call %q", ErrMissingDecision, rec.ID)
		}
	}
	return byID, nil
}
