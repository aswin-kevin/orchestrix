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
// of the task call that started them. It is private: it belongs to the run
// whose task calls these are.
var suspendedKey = state.NewKey[map[string]Suspended]("subagents.suspended", state.Private[map[string]Suspended]())

// Suspended is a subagent run that ended interrupted, kept in the parent's
// State until the parent's task call is resumed.
type Suspended struct {
	// Type is the subagent_type the run was started as.
	Type string `json:"type"`
	// ThreadID is the child run's thread id.
	ThreadID string `json:"thread_id"`
	// State is the child's Final.State, as JSON.
	State json.RawMessage `json:"state"`
	// Requests are the child's pending actions (agent.Pending of State).
	Requests []agent.ActionRequest `json:"requests"`
}

// suspendDecisions are the answers the parent's task call accepts: each
// applies to every pending request of the child.
var suspendDecisions = []agent.DecisionType{agent.Approve, agent.Reject}

// suspend keeps an interrupted child in the parent's State and pauses the
// task call with one request summarizing the child's.
func suspend(c *tool.Call, run childRun, final *agent.Final) (tool.Result, error) {
	pending, ok := agent.Pending(final.State)
	if !ok {
		return tool.Errorf("subagent %s was interrupted with no pending action", run.typ), nil
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
			Reason:  approvalReason(run.typ, pending.Requests),
			Allowed: slices.Clone(suspendDecisions),
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
	s, ok := takeSuspended(c.State, c.ToolUseID)
	if !ok {
		return tool.Errorf("no suspended subagent to resume for this task call"), nil
	}
	decisions, err := childDecisions(*c.Resume, s.Requests)
	if err != nil {
		return tool.Errorf("cannot resume subagent %s: %v", s.Type, err), nil
	}
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

// takeSuspended removes and returns the child suspended under id.
func takeSuspended(s *state.State, id string) (Suspended, bool) {
	var entry Suspended
	var found bool
	suspendedKey.Update(s, func(entries map[string]Suspended) map[string]Suspended {
		entry, found = entries[id]
		if !found {
			return entries
		}
		out := maps.Clone(entries)
		delete(out, id)
		return out
	})
	return entry, found
}

// childDecisions applies the parent's decision to every pending request of
// the child: approve all, or reject all with the parent's message.
func childDecisions(resume tool.ResumeValue, requests []agent.ActionRequest) ([]agent.Decision, error) {
	typ := agent.DecisionType(resume.Type)
	if !slices.Contains(suspendDecisions, typ) {
		return nil, fmt.Errorf("unsupported decision %q", resume.Type)
	}
	decisions := make([]agent.Decision, len(requests))
	for i, req := range requests {
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
