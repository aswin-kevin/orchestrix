package subagents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// maxReasonInput bounds, in characters, the input of each child request
// quoted in the reason of the interrupt the parent raises.
const maxReasonInput = 120

// suspendedKey holds the subagents waiting for a decision, by the tool_use id
// of the task call that started them. It is run-local (state.RunLocal): it
// belongs to the run whose task calls these are, so not even a fork gets it.
var suspendedKey = state.NewKey[map[string]Suspended]("subagents.suspended", state.RunLocal[map[string]Suspended]())

// Suspended is a subagent run that ended interrupted, kept in the parent's
// State until the parent's task call is resumed. The parent's interrupt
// summarizes Requests in its Reason, with each input cut short; a host that
// wants to see exactly what it is approving reads the full requests with
// SuspendedFor.
type Suspended struct {
	// Type is the subagent_type the run was started as.
	Type string `json:"type"`
	// ThreadID is the child run's thread id.
	ThreadID string `json:"thread_id"`
	// State is the child's Final.State, as JSON.
	State json.RawMessage `json:"state"`
	// Requests are the child's pending actions (agent.Pending of State),
	// each with its full Input and the child's Reason.
	Requests []agent.ActionRequest `json:"requests"`
}

// SuspendedFor returns the subagent suspended under the task call with
// toolUseID in s, the parent's State, while that call awaits a decision
// (agent.Pending lists it). The result is a deep copy, safe to keep or
// change. It reports false when no subagent is suspended under the call.
func SuspendedFor(s *state.State, toolUseID string) (Suspended, bool) {
	entries, _ := suspendedKey.Get(s)
	entry, ok := entries[toolUseID]
	if !ok {
		return Suspended{}, false
	}
	return entry.clone(), true
}

// clone returns a deep copy of the entry.
func (e Suspended) clone() Suspended {
	e.State = bytes.Clone(e.State)
	e.Requests = slices.Clone(e.Requests)
	for i := range e.Requests {
		e.Requests[i].Input = bytes.Clone(e.Requests[i].Input)
		e.Requests[i].Allowed = slices.Clone(e.Requests[i].Allowed)
	}
	return e
}

// suspendDecisions are the answers the parent's task call can relay: each
// applies to every pending request of the child.
var suspendDecisions = []agent.DecisionType{agent.Approve, agent.Reject}

// suspend keeps an interrupted child in the parent's State and pauses the
// task call with one request summarizing the child's. It offers the
// decisions every child request accepts; when there is none, the child
// cannot be resumed through the parent, so its run is dropped and the task
// call fails instead.
func suspend(c *tool.Call, run childRun, final *agent.Final) (tool.Result, error) {
	pending, ok := agent.Pending(final.State)
	if !ok {
		return tool.Errorf("subagent %s was interrupted with no pending action", run.typ), nil
	}
	reason := approvalReason(run.typ, pending.Requests)
	allowed := relayable(pending.Requests)
	if len(allowed) == 0 {
		return tool.Errorf("%s; it needs a decision the parent cannot relay (only approve or reject), so it was stopped", reason), nil
	}
	data, err := json.Marshal(final.State)
	if err != nil {
		return tool.Errorf("cannot suspend subagent %s: %v", run.typ, err), nil
	}
	entry := Suspended{Type: run.typ, ThreadID: run.in.ThreadID, State: data, Requests: pending.Requests}
	suspendedKey.Update(c.State, func(entries map[string]Suspended) map[string]Suspended {
		out := maps.Clone(entries)
		if out == nil {
			out = make(map[string]Suspended, 1)
		}
		out[c.ToolUseID] = entry
		return out
	})
	return tool.Result{}, &agent.Interrupt{
		Requests: []agent.ActionRequest{{
			Reason:  reason,
			Allowed: allowed,
		}},
		// A Reject must reach resumeTask too: it is the child's to answer,
		// and its Suspended entry must be cleared.
		ReinvokeOnReject: true,
	}
}

// resumeTask continues the child suspended under c's task call with the
// decision c was resumed with, then settles its outcome like a fresh run's;
// it may suspend again.
func (m *middleware) resumeTask(ctx context.Context, c *tool.Call) (tool.Result, error) {
	info, ok := agent.Info(ctx)
	if !ok {
		return tool.Errorf("the task tool can only run inside an agent run"), nil
	}
	entries, _ := suspendedKey.Get(c.State)
	s, ok := entries[c.ToolUseID]
	if !ok {
		return tool.Errorf("no suspended subagent to resume for this task call"), nil
	}
	// Checked before the entry is taken, so a refused decision keeps it.
	decisions, err := childDecisions(*c.Resume, s.Requests)
	if err != nil {
		return tool.Errorf("cannot resume subagent %s: %v", s.Type, err), nil
	}
	takeSuspended(c.State, c.ToolUseID)
	childState := state.New()
	if err := json.Unmarshal(s.State, childState); err != nil {
		return tool.Errorf("cannot resume subagent %s: decode its state: %v", s.Type, err), nil
	}
	child, schema, err := m.subagent(info.Agent, s.Type)
	if err != nil {
		return tool.Errorf("cannot resume subagent %s: %v", s.Type, err), nil
	}
	run := childRun{typ: s.Type, agent: child, schema: schema, in: agent.Input{
		ThreadID:  s.ThreadID,
		State:     childState,
		Backend:   c.Backend,
		AgentName: s.Type,
		Ephemeral: true,
	}}
	// ResumeWith, not Resume: the child must keep the parent's backend and
	// stay out of the checkpointer, as on its first run.
	final := drain(ctx, child.ResumeWith(ctx, run.in, decisions))
	return m.settle(ctx, c, run, final)
}

// takeSuspended removes the child suspended under id. The Update keeps
// parallel task calls of one turn from losing each other's changes.
func takeSuspended(s *state.State, id string) {
	suspendedKey.Update(s, func(entries map[string]Suspended) map[string]Suspended {
		out := maps.Clone(entries)
		delete(out, id)
		return out
	})
}

// pruneSuspended drops the suspended children whose task call no longer
// awaits a decision: a new Run abandoned the interrupt, or the call was
// settled without the task tool (a stopped turn, a refused recheck). Without
// it their states would ride along in every checkpoint. A resumed run still
// holds its pending record when BeforeAgent runs, so its children are kept.
func pruneSuspended(s *state.State) {
	entries, ok := suspendedKey.Get(s)
	if !ok {
		return
	}
	waiting := make(map[string]bool)
	if pending, ok := agent.Pending(s); ok {
		for _, req := range pending.Requests {
			waiting[req.ToolUseID] = true
		}
	}
	kept := maps.Clone(entries)
	maps.DeleteFunc(kept, func(id string, _ Suspended) bool { return !waiting[id] })
	if len(kept) == 0 {
		suspendedKey.Delete(s)
		return
	}
	if len(kept) < len(entries) {
		suspendedKey.Set(s, kept)
	}
}

// relayable returns the decisions of suspendDecisions that every request
// accepts.
func relayable(requests []agent.ActionRequest) []agent.DecisionType {
	var out []agent.DecisionType
	for _, d := range suspendDecisions {
		if !slices.ContainsFunc(requests, func(req agent.ActionRequest) bool { return !slices.Contains(req.Allowed, d) }) {
			out = append(out, d)
		}
	}
	return out
}

// childDecisions applies the parent's decision to every pending request of
// the child: approve all, or reject all with the parent's message. It fails
// when a request does not accept the decision.
func childDecisions(resume tool.ResumeValue, requests []agent.ActionRequest) ([]agent.Decision, error) {
	typ := agent.DecisionType(resume.Type)
	if !slices.Contains(suspendDecisions, typ) {
		return nil, fmt.Errorf("unsupported decision %q", resume.Type)
	}
	decisions := make([]agent.Decision, len(requests))
	for i, req := range requests {
		if !slices.Contains(req.Allowed, typ) {
			return nil, fmt.Errorf("decision %q is not allowed for %s (allowed: %v)", typ, req.Tool, req.Allowed)
		}
		decisions[i] = agent.Decision{ToolUseID: req.ToolUseID, Type: typ}
		if typ == agent.Reject {
			decisions[i].Message = resume.Message
		}
	}
	return decisions, nil
}

// approvalReason tells the human which actions of the child are waiting:
// "Subagent <type> needs approval: <tool>(<input>); ...".
func approvalReason(typ string, requests []agent.ActionRequest) string {
	actions := make([]string, len(requests))
	for i, req := range requests {
		actions[i] = fmt.Sprintf("%s(%s)", req.Tool, compactInput(req.Input))
	}
	return fmt.Sprintf("Subagent %s needs approval: %s", typ, strings.Join(actions, "; "))
}

// compactInput is raw as compact JSON, cut to maxReasonInput characters.
func compactInput(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		// Not valid JSON: quote it as it is.
		b.Reset()
		b.Write(raw)
	}
	s := b.String()
	if utf8.RuneCountInString(s) <= maxReasonInput {
		return s
	}
	return string([]rune(s)[:maxReasonInput-1]) + "…"
}
