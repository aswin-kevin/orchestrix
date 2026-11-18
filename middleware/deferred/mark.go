package deferred

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/permission"
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
// locked (as Gate does), so it stays locked without this middleware.
//
// Policy for such a tool, once unlocked, belongs in permission rules: rules,
// hitl and hook matchers see the tool's own name, also when it is called
// through call_tool.
type Gater interface {
	// Gated reports whether the tool is locked until unlocked; false
	// treats the tool like any other.
	Gated() bool
}

// Defer returns t marked as deferred (see Deferrer). The result forwards
// every optional interface the agent's permission checks use; see Gate.
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
// The result forwards Subjecter, ShellSubjecter, PathAccessor, Editor and
// RawInputer to t, answering "", false or nil when t lacks one, so wrapping
// never weakens a permission check. It does not forward tool.Dispatcher:
// do not wrap a dispatching tool. Defer and Gate share one wrapper, so
// combining them marks a single wrapper with both flags.
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
	if a, ok := m.Tool.(tool.Availability); ok {
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

// Subject implements tool.Subjecter.
func (m *marked) Subject(in json.RawMessage) string {
	if s, ok := m.Tool.(tool.Subjecter); ok {
		return s.Subject(in)
	}
	return ""
}

// ShellSubject implements tool.ShellSubjecter.
func (m *marked) ShellSubject() bool {
	s, ok := m.Tool.(tool.ShellSubjecter)
	return ok && s.ShellSubject()
}

// Paths implements tool.PathAccessor.
func (m *marked) Paths(in json.RawMessage) []permission.PathAccess {
	if p, ok := m.Tool.(tool.PathAccessor); ok {
		return p.Paths(in)
	}
	return nil
}

// IsEdit implements tool.Editor.
func (m *marked) IsEdit(in json.RawMessage) bool {
	e, ok := m.Tool.(tool.Editor)
	return ok && e.IsEdit(in)
}

// AcceptsRawInput implements tool.RawInputer.
func (m *marked) AcceptsRawInput() bool {
	r, ok := m.Tool.(tool.RawInputer)
	return ok && r.AcceptsRawInput()
}

// lockedMessage is LockedText for the tool named name.
func lockedMessage(name string) string { return fmt.Sprintf(LockedText, name) }

// isDeferred reports whether t is marked deferred by its own Deferrer.
func isDeferred(t tool.Tool) bool {
	d, ok := t.(Deferrer)
	return ok && d.Deferred()
}

// isGated reports whether t is marked gated by its own Gater.
func isGated(t tool.Tool) bool {
	g, ok := t.(Gater)
	return ok && g.Gated()
}

// gatedLocked reports whether t is gated and not unlocked in the run with
// State s.
func gatedLocked(s *state.State, t tool.Tool) bool {
	return isGated(t) && !Unlocked(s, t.Name())
}

// Compile-time checks: the wrapper answers every interface the agent's
// permission checks and this package consult.
var (
	_ Deferrer            = (*marked)(nil)
	_ Gater               = (*marked)(nil)
	_ tool.Availability   = (*marked)(nil)
	_ tool.Subjecter      = (*marked)(nil)
	_ tool.ShellSubjecter = (*marked)(nil)
	_ tool.PathAccessor   = (*marked)(nil)
	_ tool.Editor         = (*marked)(nil)
	_ tool.RawInputer     = (*marked)(nil)
)
