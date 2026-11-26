package deferred

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// LockedText is the error result of a call of a gated tool that is still
// locked; %s is the tool's name.
const LockedText = "tool %s is not available yet: load the skill that provides it first"

// Deferrer marks a tool whose schema is withheld from the model until
// search_tools finds it (or Unlock names it). A deferred tool can still be
// called through call_tool, or directly by a model that knows its name.
type Deferrer interface {
	// Deferred reports whether the schema is withheld; false treats the
	// tool like any other.
	Deferred() bool
}

// Gater marks a tool that is hidden, unsearchable and uncallable until
// Unlock names it, such as a tool a skill unlocks when it is loaded or a
// tool of a gated MCP server. A Gater must refuse its own calls while
// locked (as Gate does), so it stays locked without this middleware, and
// must implement tool.Availability reporting the lock, so the agent refuses
// a locked call before showing it to an Approver. Gate does both.
//
// Policy for such a tool, once unlocked, belongs in permission rules: rules,
// hitl and hook matchers see the tool's own name, also when it is called
// through call_tool.
type Gater interface {
	// Gated reports whether the tool is locked until unlocked; false
	// treats the tool like any other.
	Gated() bool
}

// Defer returns t marked as deferred (see Deferrer). The result unwraps to t
// (see tool.As); see Gate.
func Defer(t tool.Tool) tool.Tool {
	if t == nil {
		return nil
	}
	m := mark(t)
	m.deferred = true
	return m
}

// Gate returns t marked as gated (see Gater). Its Call returns
// tool.Errorf(LockedText, name) while Unlocked(c.State, name) is false, and
// it implements tool.Availability, so the agent refuses a locked call before
// asking for any approval.
//
// The result unwraps to t (see tool.As), so every capability the agent's
// permission checks find with tool.As (ReadOnlyer, PermissionChecker,
// Subjecter, PathAccessor, ...) is t's own, and wrapping never weakens a
// permission check. That includes tool.Dispatcher: a gated dispatcher,
// once unlocked, dispatches to its target (a locked one is refused before
// dispatch, by Available), and like any dispatcher it cannot be the target
// of another dispatch. Defer and Gate share one wrapper, so combining them
// marks a single wrapper with both flags.
func Gate(t tool.Tool) tool.Tool {
	if t == nil {
		return nil
	}
	m := mark(t)
	m.gated = true
	return m
}

// marked is the one wrapper Defer and Gate produce.
type marked struct {
	tool.Tool
	deferred, gated bool
}

// mark returns a wrapper of t to set flags on: a copy of t when t already is
// one, so the flags combine instead of nesting.
func mark(t tool.Tool) *marked {
	if m, ok := t.(*marked); ok {
		c := *m
		return &c
	}
	return &marked{Tool: t}
}

// Deferred implements Deferrer: set by Defer, or by the wrapped tool's own
// Deferrer.
func (m *marked) Deferred() bool { return m.deferred || isDeferred(m.Tool) }

// Gated implements Gater: set by Gate, or by the wrapped tool's own Gater.
func (m *marked) Gated() bool { return m.gated || isGated(m.Tool) }

// Available implements tool.Availability: a locked gated tool is
// unavailable, and so is one the wrapped tool's own Availability refuses.
func (m *marked) Available(s *state.State) error {
	if gatedLocked(s, m) {
		return errors.New(lockedMessage(m.Name()))
	}
	if a, ok := tool.As[tool.Availability](m.Tool); ok {
		return a.Available(s)
	}
	return nil
}

// Call runs the wrapped tool unless it is gated and locked in c's run.
func (m *marked) Call(ctx context.Context, in json.RawMessage, c *tool.Call) (tool.Result, error) {
	var s *state.State
	if c != nil {
		s = c.State
	}
	if gatedLocked(s, m) {
		return tool.Errorf("%s", lockedMessage(m.Name())), nil
	}
	return m.Tool.Call(ctx, in, c)
}

// Unwrap returns the wrapped tool, whose capabilities tool.As finds.
func (m *marked) Unwrap() tool.Tool { return m.Tool }

// lockedMessage is LockedText for the tool named name.
func lockedMessage(name string) string { return fmt.Sprintf(LockedText, name) }

// isDeferred reports whether t is marked deferred by its own Deferrer, found
// with tool.As.
func isDeferred(t tool.Tool) bool {
	d, ok := tool.As[Deferrer](t)
	return ok && d.Deferred()
}

// isGated reports whether t is marked gated by its own Gater, found with
// tool.As.
func isGated(t tool.Tool) bool {
	g, ok := tool.As[Gater](t)
	return ok && g.Gated()
}

// gatedLocked reports whether t is gated and not unlocked in the run with
// State s.
func gatedLocked(s *state.State, t tool.Tool) bool {
	return isGated(t) && !Unlocked(s, t.Name())
}

// Compile-time checks: the wrapper answers the markers of this package and
// tool.Availability itself, and unwraps for everything else.
var (
	_ Deferrer                        = (*marked)(nil)
	_ Gater                           = (*marked)(nil)
	_ tool.Availability               = (*marked)(nil)
	_ interface{ Unwrap() tool.Tool } = (*marked)(nil)
)
