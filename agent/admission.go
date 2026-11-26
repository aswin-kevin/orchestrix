package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aswin-kevin/orchestrix/internal/toolutil"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Permission admission: how each call of a turn is checked against the
// permission rules, the mode, the tool's own check and the Approver before it
// may run.

// start decides how c proceeds. It returns nil for a call that may run, or
// the result of a call that must not: a blocked one, one the run's
// cancellation reached first, or one permission refused. A call that needs
// an approval no Approver can give is marked pending instead.
func (r *run) start(c *toolCall) (res *tool.Result) {
	defer func() {
		if v := recover(); v != nil {
			res = panicResult(c.name, v)
		}
	}()

	if c.blocked != nil {
		return c.blocked
	}
	if stopped := r.stoppedResult(); stopped != nil {
		return stopped
	}
	if c.resumed != nil {
		return r.recheck(c)
	}
	return r.admit(c)
}

// admit checks the permission of a new call: see start for the outcomes. An
// input rewrite carried by an Allow decision is applied to c.
func (r *run) admit(c *toolCall) *tool.Result {
	decision, req := r.evaluate(c)
	if decision.Behavior == permission.Ask {
		if r.agent.cfg.Approver == nil {
			c.interrupt = askInterrupt(c, decision.Message)
			return nil
		}
		decision = r.agent.cfg.Approver(r.ctx, req, decision.Message)
		c.approved = decision.Behavior == permission.Allow
	}
	return r.applyPermission(c, decision)
}

// recheck evaluates the permission of a call a human decided to run, on the
// input it will run with: an edit may point the call somewhere the rules
// forbid. The human's decision is the approval, so an Ask counts as granted;
// a Deny still refuses the call.
func (r *run) recheck(c *toolCall) *tool.Result {
	c.approved = true
	decision, _ := r.evaluate(c)
	if decision.Behavior == permission.Ask {
		decision = permission.Allowed()
	}
	return r.applyPermission(c, decision)
}

// applyPermission turns a final permission decision into start's outcome:
// a refusal becomes an error result, and an Allow's input rewrite is applied
// to c and validated.
func (r *run) applyPermission(c *toolCall, decision permission.Decision) *tool.Result {
	if decision.Behavior != permission.Allow {
		return toolutil.ErrorResult("Permission denied: %s", decision.Message)
	}
	if len(decision.UpdatedInput) == 0 {
		return nil
	}
	c.input = decision.UpdatedInput
	return validateInput(c.tool, c.input)
}

// evaluate runs the permission pipeline on c and returns its decision and
// c's permission request (the one an Approver is asked about). A dispatched
// call is evaluated on its dispatcher first, with the dispatcher's own
// input, so that rules naming the dispatcher apply: its deny refuses the
// call before the target's request is even built (the request is then
// zero), and its ask makes the call need approval even where the target
// alone would be allowed. Otherwise the target's decision stands. An input
// rewrite in the dispatcher's allow is ignored: its input has already been
// dispatched.
func (r *run) evaluate(c *toolCall) (permission.Decision, permission.Request) {
	pc := r.permissionContext()
	own := permission.Allowed()
	if c.dispatcher != nil {
		own = permission.Evaluate(r.agent.permissionRequest(r.ctx, r.state, c.via, c.dispatcher, c.dispatchInput), pc)
	}
	if own.Behavior == permission.Deny {
		return own, permission.Request{}
	}
	req := r.permissionRequest(c)
	decision := permission.Evaluate(req, pc)
	if own.Behavior == permission.Ask && decision.Behavior == permission.Allow {
		return permission.AskUser(own.Message), req
	}
	return decision, req
}

// permissionRequest describes c for permission.Evaluate, under the run's
// current permission mode.
func (r *run) permissionRequest(c *toolCall) permission.Request {
	return r.agent.permissionRequest(r.ctx, r.state, c.name, c.tool, c.input)
}

// permissionContext is what the run's calls are evaluated under. It is read
// afresh for every evaluation, so a mode a call sets (SetPermissionMode)
// applies to the calls admitted after it.
func (r *run) permissionContext() permission.Context {
	return r.agent.permissionContext(r.state)
}

// CheckPermission runs the permission pipeline the loop applies to a new
// call (rules, permission mode and the tool's own check) on call.Input, and
// returns its decision. The mode is that of call.State (see
// PermissionMode); a nil State means Config.PermissionMode. It never
// consults the Approver: an Ask is returned as is, for the caller to
// resolve, which lets a ToolWrapper ask a human itself. call.Tool is used
// when set, else the tool named call.Name (from the current turn's tools
// when ctx belongs to a run of a); a name the agent does not know is
// denied. A panic in the tool's methods propagates to the caller.
func (a *Agent) CheckPermission(ctx context.Context, call *ToolCall) permission.Decision {
	t, ok := a.toolFor(ctx, call)
	if !ok {
		return permission.Denied(fmt.Sprintf("unknown tool %q", call.Name))
	}
	return permission.Evaluate(a.permissionRequest(ctx, call.State, call.Name, t, call.Input), a.permissionContext(call.State))
}

// toolFor resolves the tool a ToolCall built outside the loop refers to:
// call.Tool when set, else the tool named call.Name of the current turn
// when ctx belongs to a run of a (see RunInfo.Tool), else the agent's tool
// of that name (see Tool). ok is false for a name the agent does not know.
func (a *Agent) toolFor(ctx context.Context, call *ToolCall) (t tool.Tool, ok bool) {
	if call.Tool != nil {
		return call.Tool, true
	}
	if info, inRun := Info(ctx); inRun && info.Agent == a {
		return info.Tool(call.Name)
	}
	return a.Tool(call.Name)
}

// permissionRequest describes a call of t named name with input in, made by
// the run with State s, for permission.Evaluate, using whichever optional
// interfaces tool.As finds in the tool. ctx and the run's permission context
// are passed to the tool's own CheckPermission.
func (a *Agent) permissionRequest(ctx context.Context, s *state.State, name string, t tool.Tool, in json.RawMessage) permission.Request {
	req := permission.Request{
		Tool:  name,
		Input: in,
	}
	if r, ok := tool.As[tool.ReadOnlyer](t); ok {
		req.ReadOnly = r.ReadOnly(in)
	}
	if c, ok := tool.As[tool.PermissionChecker](t); ok {
		req.SelfCheck = c.CheckPermission(ctx, in, a.permissionContext(s))
	}
	if e, ok := tool.As[tool.Editor](t); ok {
		req.Edit = e.IsEdit(in)
	}
	if sub, ok := tool.As[tool.Subjecter](t); ok {
		req.Subject = sub.Subject(in)
	}
	if sh, ok := tool.As[tool.ShellSubjecter](t); ok {
		req.Shell = sh.ShellSubject()
	}
	if p, ok := tool.As[tool.PathAccessor](t); ok {
		req.Paths = p.Paths(in)
	}
	return req
}

// permissionContext is the permission context of a run with State s: its
// current mode (see PermissionMode) and the configured rules.
func (a *Agent) permissionContext(s *state.State) permission.Context {
	return permission.Context{Mode: a.PermissionMode(s), Rules: a.cfg.Rules}
}

// hold handles a call that may not start because the turn halted. When there
// is no Approver, its permission is still evaluated so that every approval
// the turn needs is asked for in the same interrupt. Anything else is left
// not started and evaluated afresh on resume.
func (r *run) hold(c *toolCall) {
	if c.blocked != nil || r.agent.cfg.Approver != nil {
		return
	}
	decision, ok := r.heldDecision(c)
	if ok && decision.Behavior == permission.Ask {
		c.interrupt = askInterrupt(c, decision.Message)
		r.announce(c)
	}
}

// heldDecision evaluates the permission of a held call. The tools' methods
// may panic; that leaves the call not started (ok is false), and it settles
// with a panic result once it is scheduled for real.
func (r *run) heldDecision(c *toolCall) (decision permission.Decision, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	decision, _ = r.evaluate(c)
	return decision, true
}
