package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aswin-kevin/orchestrix/permission"
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
	req := r.permissionRequest(c)
	decision := permission.Evaluate(req, r.agent.permissionContext())
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
	decision := permission.Evaluate(r.permissionRequest(c), r.agent.permissionContext())
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
		return errorResult("Permission denied: %s", decision.Message)
	}
	if len(decision.UpdatedInput) == 0 {
		return nil
	}
	c.input = decision.UpdatedInput
	return validateInput(c.tool, c.input)
}

// permissionRequest describes c for permission.Evaluate.
func (r *run) permissionRequest(c *toolCall) permission.Request {
	return r.agent.permissionRequest(r.ctx, c.name, c.tool, c.input)
}

// CheckPermission runs the permission pipeline the loop applies to a new
// call (rules, permission mode and the tool's own check) on call.Input, and
// returns its decision. It never consults the Approver: an Ask is returned
// as is, for the caller to resolve, which lets a ToolWrapper ask a human
// itself. call.Tool is used when set, else the agent's tool named call.Name;
// a name the agent does not know is denied. A panic in the tool's methods
// propagates to the caller.
func (a *Agent) CheckPermission(ctx context.Context, call *ToolCall) permission.Decision {
	t := call.Tool
	if t == nil {
		var ok bool
		if t, ok = a.registry.Get(call.Name); !ok {
			return permission.Denied(fmt.Sprintf("unknown tool %q", call.Name))
		}
	}
	return permission.Evaluate(a.permissionRequest(ctx, call.Name, t, call.Input), a.permissionContext())
}

// permissionRequest describes a call of t named name with input in for
// permission.Evaluate, using whichever optional interfaces the tool
// implements. ctx is passed to the tool's own CheckPermission.
func (a *Agent) permissionRequest(ctx context.Context, name string, t tool.Tool, in json.RawMessage) permission.Request {
	req := permission.Request{
		Tool:      name,
		Input:     in,
		ReadOnly:  t.ReadOnly(in),
		SelfCheck: t.CheckPermission(ctx, in, a.permissionContext()),
	}
	if e, ok := t.(tool.Editor); ok {
		req.Edit = e.IsEdit(in)
	}
	if s, ok := t.(tool.Subjecter); ok {
		req.Subject = s.Subject(in)
	}
	if s, ok := t.(tool.ShellSubjecter); ok {
		req.Shell = s.ShellSubject()
	}
	if p, ok := t.(tool.PathAccessor); ok {
		req.Paths = p.Paths(in)
	}
	return req
}

func (a *Agent) permissionContext() permission.Context {
	return permission.Context{Mode: a.cfg.PermissionMode, Rules: a.cfg.Rules}
}

// hold handles a call that may not start because the turn halted. When there
// is no Approver, its permission is still evaluated so that every approval
// the turn needs is asked for in the same interrupt. Anything else is left
// not started and evaluated afresh on resume.
func (r *run) hold(c *toolCall) {
	if c.blocked != nil || r.agent.cfg.Approver != nil {
		return
	}
	req, ok := r.heldPermissionRequest(c)
	if !ok {
		return
	}
	decision := permission.Evaluate(req, r.agent.permissionContext())
	if decision.Behavior == permission.Ask {
		c.interrupt = askInterrupt(c, decision.Message)
		r.announce(c)
	}
}

// heldPermissionRequest is permissionRequest for a held call. The tool's
// methods may panic; that leaves the call not started (ok is false), and it
// settles with a panic result once it is scheduled for real.
func (r *run) heldPermissionRequest(c *toolCall) (req permission.Request, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return r.permissionRequest(c), true
}
