// Package hitl adds human-in-the-loop approval to chosen tools, configured
// per tool: which decisions a human may answer with, and an optional
// predicate narrowing which calls need one.
//
// It sits on top of the permission pipeline and does not replace it: a
// configured call pauses even in bypass mode, but a deny rule still refuses
// it first, and a call a human already approved in the permission step is not
// asked about again. The pause itself is the agent's ordinary interrupt: the
// host answers it with Agent.Resume.
package hitl

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// DefaultAllowed is the set of decisions an InterruptConfig offers when its
// Allowed is nil.
var DefaultAllowed = []agent.DecisionType{agent.Approve, agent.Edit, agent.Reject}

// approvedKey holds the tool_use ids hitl already cleared in this run. A tool
// resumed after that is answering an interrupt it raised itself, and must
// keep its own Resume value.
var approvedKey = state.NewKey[[]string]("hitl.approved", state.Private[[]string]())

// askedKey holds the tool_use ids hitl interrupted and has not yet seen
// answered, so a resumed call is recognized even when its edited input no
// longer satisfies When.
var askedKey = state.NewKey[[]string]("hitl.asked", state.Private[[]string]())

// InterruptConfig describes when and how one tool needs a human decision.
type InterruptConfig struct {
	// Allowed lists the decisions the host may answer with. Nil means
	// DefaultAllowed.
	Allowed []agent.DecisionType
	// When reports whether a call needs a decision, judging by its Input. Nil
	// means every call does.
	When func(ctx context.Context, call *agent.ToolCall) bool
	// Reason tells the human why a decision is needed. Empty means
	// "Approval required for <tool>".
	Reason string
}

// Config configures the middleware.
type Config struct {
	// InterruptOn maps a tool name to its InterruptConfig. A tool that is not
	// listed is left alone; nil lists none.
	InterruptOn map[string]InterruptConfig
}

// middleware is the hitl middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type middleware struct{ cfg Config }

// New returns the hitl middleware for cfg.
func New(cfg Config) agent.Middleware { return &middleware{cfg: cfg} }

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "hitl" }

// WrapTool implements agent.ToolWrapper.
//
// A configured Approver may be called concurrently for parallel tool calls,
// and the call holds its concurrency slot while it waits, so it must be safe
// for concurrent use and should not block indefinitely.
func (m *middleware) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	cfg, ok := m.cfg.InterruptOn[call.Name]
	switch {
	case !ok || cleared(call):
		return next(ctx, call)
	case call.Call.Resume != nil && asked(call):
		// Answering hitl's own interrupt: When judged the original input,
		// and an edit may have changed what it says.
		return resumed(ctx, call, next)
	case cfg.When != nil && !cfg.When(ctx, call):
		return next(ctx, call)
	case call.Approved:
		return next(ctx, call) // the permission step already asked a human
	}
	if a, approver := approverOf(ctx); approver != nil {
		return ask(ctx, a, approver, cfg, call, next)
	}
	askedKey.Update(call.State, func(ids []string) []string { return append(ids, call.ID) })
	return tool.Result{}, interruptFor(cfg, call.Name)
}

// cleared reports whether hitl already let this call through.
func cleared(call *agent.ToolCall) bool {
	ids, _ := approvedKey.Get(call.State)
	return slices.Contains(ids, call.ID)
}

// asked reports whether hitl raised an interrupt for this call that is still
// unanswered.
func asked(call *agent.ToolCall) bool {
	ids, _ := askedKey.Get(call.State)
	return slices.Contains(ids, call.ID)
}

// resumed carries out the human's decision on hitl's own interrupt. The loop
// has already applied an edit's new input, schema-checked it and checked it
// against the permission rules again.
func resumed(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	resume := call.Call.Resume
	call.Call.Resume = nil // consumed: the tool must not mistake it for its own
	askedKey.Update(call.State, func(ids []string) []string { return without(ids, call.ID) })
	if resume.Type == tool.ResumeRespond {
		return tool.Text(resume.Message), nil
	}
	approvedKey.Update(call.State, func(ids []string) []string { return append(ids, call.ID) })
	return next(ctx, call)
}

// without returns ids minus id, without touching ids.
func without(ids []string, id string) []string {
	return slices.DeleteFunc(slices.Clone(ids), func(s string) bool { return s == id })
}

// ask puts the question to approver, the Approver of agent a running the
// call, and waits for the answer. The question describes the call as the
// permission step does (paths, subject, edit). A rewrite of the input is
// honored only where the host may edit, and is checked against the
// permission rules like any edited call.
func ask(ctx context.Context, a *agent.Agent, approver permission.Approver, cfg InterruptConfig, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	decision := approver(ctx, a.PermissionRequest(ctx, call), reasonFor(cfg, call.Name))
	if decision.Behavior != permission.Allow {
		return rejected(decision.Message), nil
	}
	if len(decision.UpdatedInput) > 0 && slices.Contains(allowedOf(cfg), agent.Edit) {
		if res := applyRewrite(ctx, a, call, decision.UpdatedInput); res != nil {
			return *res, nil
		}
	}
	return next(ctx, call)
}

// applyRewrite makes in the call's input after checking it against the
// tool's schema and the permission rules. It returns an error result when the
// rewrite is refused. The Approver's allow stands in for an Ask, but a deny
// rule (or the mode) of agent a still wins.
func applyRewrite(ctx context.Context, a *agent.Agent, call *agent.ToolCall, in json.RawMessage) *tool.Result {
	if err := tool.CheckInput(call.Tool, in); err != nil {
		res := tool.Errorf("invalid input for %s: %v", call.Name, err)
		return &res
	}
	edited := *call
	edited.Input = in
	if d := a.CheckPermission(ctx, &edited); d.Behavior == permission.Deny {
		res := tool.Errorf("Permission denied: %s", d.Message)
		return &res
	}
	call.Input = in
	return nil
}

// rejected is the result of a call the Approver refused.
func rejected(message string) tool.Result {
	if message == "" {
		return tool.Errorf("Rejected.")
	}
	return tool.Errorf("Rejected: %s", message)
}

// approverOf returns the agent running ctx and its Approver; the Approver
// is nil when the agent has none or ctx is not inside a run.
func approverOf(ctx context.Context) (*agent.Agent, permission.Approver) {
	info, ok := agent.Info(ctx)
	if !ok {
		return nil, nil
	}
	return info.Agent, info.Agent.Config().Approver
}

// interruptFor builds the interrupt that asks the host about a call of name.
func interruptFor(cfg InterruptConfig, name string) *agent.Interrupt {
	return &agent.Interrupt{Requests: []agent.ActionRequest{{
		Reason:  reasonFor(cfg, name),
		Allowed: slices.Clone(allowedOf(cfg)),
	}}}
}

// allowedOf returns the decisions cfg offers.
func allowedOf(cfg InterruptConfig) []agent.DecisionType {
	if cfg.Allowed == nil {
		return DefaultAllowed
	}
	return cfg.Allowed
}

func reasonFor(cfg InterruptConfig, name string) string {
	if cfg.Reason != "" {
		return cfg.Reason
	}
	return "Approval required for " + name
}
