package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
)

// permissionModeKey holds a run's permission mode when it was changed during
// the run (plan mode, for example). Unset means Config.PermissionMode.
//
// It is part of State, so it is checkpointed and restored with the thread,
// and State.Fork copies it, so a fork continues in the same mode. It is
// private: State.Child does not copy it, so a subagent does not inherit it
// implicitly (the subagents middleware derives the subagent's mode from it).
var permissionModeKey = state.NewKey[permission.Mode]("agent.permission_mode", state.Private[permission.Mode]())

// ErrInvalidMode is returned by SetPermissionMode for a mode
// permission.Mode.Valid rejects.
var ErrInvalidMode = errors.New("agent: invalid permission mode")

// PermissionMode returns the permission mode a run with State s is in: the
// mode stored by SetPermissionMode when set, else Config.PermissionMode. A
// nil s yields Config.PermissionMode.
func (a *Agent) PermissionMode(s *state.State) permission.Mode {
	if s != nil {
		if mode, ok := PermissionModeOf(s); ok {
			return mode
		}
	}
	return a.cfg.PermissionMode
}

// SetPermissionMode changes the permission mode of the run with State s. It
// takes effect at the next permission evaluation. A tool that calls it must
// not be concurrency-safe: calls that are not concurrency-safe run one at a
// time, and the calls after one are admitted only once it has finished, so
// the new mode applies to the next call of the same turn. From a
// concurrency-safe tool, calls of the same parallel batch may already have
// been admitted under the old mode.
//
// The mode is part of State: it is checkpointed with the thread and
// State.Fork copies it, but State.Child does not, so a subagent does not
// inherit it implicitly. An empty m clears it, returning the run to
// Config.PermissionMode; a mode Valid rejects returns an error wrapping
// ErrInvalidMode and leaves s unchanged. A nil s is a programming error and
// panics.
//
// Any code that can write the run's State, a tool or middleware included,
// can call it and so holds full authority over the run's mode, up to
// bypassPermissions: give that ability only to code trusted with it.
func SetPermissionMode(s *state.State, m permission.Mode) error {
	if s == nil {
		panic("agent: SetPermissionMode called with a nil State")
	}
	switch {
	case m == "":
		permissionModeKey.Delete(s)
	case !m.Valid():
		return fmt.Errorf("%w %q", ErrInvalidMode, m)
	default:
		permissionModeKey.Set(s, m)
	}
	return nil
}

// PermissionModeOf returns the mode stored in s by SetPermissionMode and
// true, or "" and false when none is stored (the run then uses its
// Config.PermissionMode). Code without the run's Agent, such as middleware
// called outside a run, uses it; with the Agent, Agent.PermissionMode gives
// the effective mode. A nil s yields "" and false.
func PermissionModeOf(s *state.State) (permission.Mode, bool) {
	if s == nil {
		return "", false
	}
	return permissionModeKey.Get(s)
}

// PermissionRequest describes call for permission.Evaluate and Approvers
// exactly as the loop does: ReadOnly, Edit, Subject, Shell, Paths and the
// tool's own SelfCheck, made under the mode of call.State (see
// PermissionMode). call.Tool is used when set, else the tool named
// call.Name (from the current turn's tools when ctx belongs to a run of a);
// for a name the agent does not know, only Tool and Input are set. ctx is
// passed to the tool's own CheckPermission, and a panic in the tool's
// methods propagates to the caller.
func (a *Agent) PermissionRequest(ctx context.Context, call *ToolCall) permission.Request {
	t, ok := a.toolFor(ctx, call)
	if !ok {
		return permission.Request{Tool: call.Name, Input: call.Input}
	}
	return a.permissionRequest(ctx, call.State, call.Name, t, call.Input)
}
