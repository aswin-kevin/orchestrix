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
	"fmt"
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
func (m *middleware) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	cfg, ok := m.cfg.InterruptOn[call.Name]
	if !ok || (cfg.When != nil && !cfg.When(ctx, call)) || m.cleared(call) {
		return next(ctx, call)
	}
	if call.Call.Resume != nil {
		return m.resumed(ctx, call, next)
	}
	if call.Approved {
		return next(ctx, call) // the permission step already asked a human
	}
	if approver := approverOf(ctx); approver != nil {
		return m.ask(ctx, approver, cfg, call, next)
	}
	return tool.Result{}, interruptFor(cfg, call.Name)
}

// cleared reports whether hitl already let this call through.
func (m *middleware) cleared(call *agent.ToolCall) bool {
	ids, _ := approvedKey.Get(call.State)
	return slices.Contains(ids, call.ID)
}

// resumed carries out the human's decision on hitl's own interrupt. The loop
// has already applied an edit's new input, schema-checked it and checked it
// against the permission rules again.
func (m *middleware) resumed(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	resume := call.Call.Resume
	if resume.Type == tool.ResumeRespond {
		return tool.Text(resume.Message), nil
	}
	approvedKey.Update(call.State, func(ids []string) []string { return append(ids, call.ID) })
	call.Call.Resume = nil
	return next(ctx, call)
}

// ask puts the question to the run's Approver and waits for the answer.
func (m *middleware) ask(ctx context.Context, approver permission.Approver, cfg InterruptConfig, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	req := permission.Request{Tool: call.Name, Input: call.Input}
	decision := approver(ctx, req, reasonFor(cfg, call.Name))
	if decision.Behavior != permission.Allow {
		return tool.Errorf("Rejected: %s", decision.Message), nil
	}
	if len(decision.UpdatedInput) > 0 {
		if err := checkInput(call.Tool, decision.UpdatedInput); err != nil {
			return tool.Errorf("invalid input for %s: %v", call.Name, err), nil
		}
		call.Input = decision.UpdatedInput
	}
	return next(ctx, call)
}

// approverOf returns the Approver of the agent running ctx, or nil.
func approverOf(ctx context.Context) permission.Approver {
	info, ok := agent.Info(ctx)
	if !ok {
		return nil
	}
	return info.Agent.Config().Approver
}

// interruptFor builds the interrupt that asks the host about a call of name.
func interruptFor(cfg InterruptConfig, name string) *agent.Interrupt {
	allowed := cfg.Allowed
	if allowed == nil {
		allowed = DefaultAllowed
	}
	return &agent.Interrupt{Requests: []agent.ActionRequest{{
		Reason:  reasonFor(cfg, name),
		Allowed: slices.Clone(allowed),
	}}}
}

func reasonFor(cfg InterruptConfig, name string) string {
	if cfg.Reason != "" {
		return cfg.Reason
	}
	return "Approval required for " + name
}

// checkInput validates in against t's schema, unless t takes raw input.
func checkInput(t tool.Tool, in json.RawMessage) error {
	if raw, ok := t.(tool.RawInputer); ok && raw.AcceptsRawInput() {
		return nil
	}
	if err := tool.Validate(t.Schema(), in); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	return nil
}
