package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
)

// PermissionModeKey holds a run's permission mode when it was changed during
// the run (plan mode, for example). Unset means Config.PermissionMode.
//
// It is part of State, so it is checkpointed and restored with the thread,
// and State.Fork copies it, so a fork continues in the same mode. It is
// private: State.ChildFor does not copy it, so a subagent does not inherit it
// implicitly (the subagents middleware derives the subagent's mode from it).
//
// Any code that can write the run's State, a tool or middleware included,
// can set this key and so holds full authority over the run's mode, up to
// bypassPermissions: give that ability only to code trusted with it.
var PermissionModeKey = state.NewKey[permission.Mode]("agent.permission_mode", state.Private[permission.Mode]())

// ErrInvalidMode is returned by SetPermissionMode for a mode
// permission.Mode.Valid rejects.
var ErrInvalidMode = errors.New("agent: invalid permission mode")

// PermissionMode returns the permission mode a run with State s is in: the
// mode stored under PermissionModeKey when set, else Config.PermissionMode.
// A nil s yields Config.PermissionMode.
func (a *Agent) PermissionMode(s *state.State) permission.Mode {
	if s != nil {
		if mode, ok := PermissionModeKey.Get(s); ok {
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
// An empty m deletes the key, returning the run to Config.PermissionMode; a
// mode Valid rejects returns an error wrapping ErrInvalidMode and leaves s
// unchanged. A nil s is a programming error and panics.
func SetPermissionMode(s *state.State, m permission.Mode) error {
	if s == nil {
		panic("agent: SetPermissionMode called with a nil State")
	}
	switch {
	case m == "":
		PermissionModeKey.Delete(s)
	case !m.Valid():
		return fmt.Errorf("%w %q", ErrInvalidMode, m)
	default:
		PermissionModeKey.Set(s, m)
	}
	return nil
}

// PermissionRequest describes call for permission.Evaluate and Approvers
// exactly as the loop does: ReadOnly, Edit, Subject, Shell, Paths and the
// tool's own SelfCheck, made under the mode of call.State (see
// PermissionMode). call.Tool is used when set, else the agent's tool named
// call.Name; for a name the agent does not know, only Tool and Input are
// set. ctx is passed to the tool's own CheckPermission, and a panic in the
// tool's methods propagates to the caller.
func (a *Agent) PermissionRequest(ctx context.Context, call *ToolCall) permission.Request {
	t, ok := a.toolFor(call)
	if !ok {
		return permission.Request{Tool: call.Name, Input: call.Input}
	}
	return a.permissionRequest(ctx, call.State, call.Name, t, call.Input)
}
