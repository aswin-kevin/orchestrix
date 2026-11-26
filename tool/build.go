package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aswin-kevin/orchestrix/internal/names"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
)

// Spec describes a tool declaratively. Every optional field defaults to the
// most restrictive behaviour, so forgetting one never widens what a tool may
// do.
type Spec struct {
	// Name must be non-empty and use only [A-Za-z0-9_.:-].
	Name string
	// Description tells the model what the tool does.
	Description string
	// Schema is the input schema; nil means a tool without parameters.
	Schema map[string]any

	// ReadOnly, Concurrent and Edit default to false when nil.
	ReadOnly func(json.RawMessage) bool
	// Concurrent reports whether a call with this input may run in parallel
	// with other calls; nil means it may not.
	Concurrent func(json.RawMessage) bool
	// Edit reports whether the call modifies files, for the accept-edits
	// permission mode; nil means it does not.
	Edit func(json.RawMessage) bool
	// Subject returns the string permission rules match against (a shell
	// command, a URL); nil means none.
	Subject func(json.RawMessage) string
	// ShellSubject marks Subject as a shell command line, so permission
	// patterns match each command in it; false means patterns match Subject
	// as plain text.
	ShellSubject bool
	// Paths must report every file-system path the call touches, as virtual
	// absolute paths, because permission checks see nothing else. Nil means
	// the tool touches no paths.
	Paths func(json.RawMessage) []permission.PathAccess
	// Permission is the tool's own verdict (see PermissionChecker); nil
	// means no opinion, which leaves the call to the rules and the mode and
	// otherwise asks the user.
	Permission func(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision
	// Run executes the tool and is required.
	Run func(ctx context.Context, in json.RawMessage, c *Call) (Result, error)
	// RawInput marks the tool as accepting input that bypasses validation.
	RawInput bool
	// Dispatch makes the tool a Dispatcher that stands for the tool it
	// names; nil means the tool is not a Dispatcher.
	Dispatch func(in json.RawMessage) (name string, input json.RawMessage, err error)
	// Available reports whether a run with the given State may call the
	// tool (see Availability); nil means it always may.
	Available func(s *state.State) error
}

// Build turns a Spec into a Tool. It panics on an invalid name or a missing
// Run, which are programming errors best caught at start-up. The tool
// implements every optional capability interface of this package, except
// Dispatcher, which it implements only when Spec.Dispatch is set.
func Build(s Spec) Tool {
	if err := ValidName(s.Name); err != nil {
		panic(fmt.Sprintf("tool: Build: %v", err))
	}
	if s.Run == nil {
		panic(fmt.Sprintf("tool: Build: tool %q has no Run function", s.Name))
	}
	if s.Schema == nil {
		s.Schema = emptyObjectSchema()
	}
	b := &built{spec: s}
	if s.Dispatch != nil {
		return &builtDispatcher{b}
	}
	return b
}

// built adapts a Spec to Tool and to every optional interface but
// Dispatcher; absent funcs answer with the fail-closed default.
type built struct{ spec Spec }

// builtDispatcher is a built tool whose Spec sets Dispatch, so it is a
// Dispatcher too. Dispatcher is the one capability whose mere presence
// changes behaviour, so built does not implement it unconditionally.
type builtDispatcher struct{ *built }

// Dispatch implements Dispatcher.
func (d *builtDispatcher) Dispatch(in json.RawMessage) (string, json.RawMessage, error) {
	return d.spec.Dispatch(in)
}

func (b *built) Name() string           { return b.spec.Name }
func (b *built) Description() string    { return b.spec.Description }
func (b *built) Schema() map[string]any { return b.spec.Schema }
func (b *built) AcceptsRawInput() bool  { return b.spec.RawInput }

func (b *built) ReadOnly(in json.RawMessage) bool        { return check(b.spec.ReadOnly, in) }
func (b *built) ConcurrencySafe(in json.RawMessage) bool { return check(b.spec.Concurrent, in) }
func (b *built) IsEdit(in json.RawMessage) bool          { return check(b.spec.Edit, in) }

func (b *built) Subject(in json.RawMessage) string {
	if b.spec.Subject == nil {
		return ""
	}
	return b.spec.Subject(in)
}

func (b *built) ShellSubject() bool { return b.spec.ShellSubject }

func (b *built) Paths(in json.RawMessage) []permission.PathAccess {
	if b.spec.Paths == nil {
		return nil
	}
	return b.spec.Paths(in)
}

func (b *built) CheckPermission(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision {
	if b.spec.Permission == nil {
		return permission.Decision{}
	}
	return b.spec.Permission(ctx, in, pc)
}

func (b *built) Available(s *state.State) error {
	if b.spec.Available == nil {
		return nil
	}
	return b.spec.Available(s)
}

func (b *built) Call(ctx context.Context, in json.RawMessage, c *Call) (Result, error) {
	return b.spec.Run(ctx, in, c)
}

func check(f func(json.RawMessage) bool, in json.RawMessage) bool {
	return f != nil && f(in)
}

// Option adjusts the Spec that New builds.
type Option func(*Spec)

func always(json.RawMessage) bool { return true }

// WithReadOnly marks the tool as read-only.
func WithReadOnly() Option { return func(s *Spec) { s.ReadOnly = always } }

// WithConcurrent marks the tool as safe to run alongside other tools.
func WithConcurrent() Option { return func(s *Spec) { s.Concurrent = always } }

// WithEdit marks the tool as a file editor.
func WithEdit() Option { return func(s *Spec) { s.Edit = always } }

// WithPermission sets the tool's own permission verdict.
func WithPermission(f func(context.Context, json.RawMessage, permission.Context) permission.Decision) Option {
	return func(s *Spec) { s.Permission = f }
}

// WithSubject sets the string permission rules match against.
func WithSubject(f func(json.RawMessage) string) Option {
	return func(s *Spec) { s.Subject = f }
}

// WithShellSubject sets the subject permission rules match against and
// marks it as a shell command line; see Spec.ShellSubject.
func WithShellSubject(f func(json.RawMessage) string) Option {
	return func(s *Spec) {
		s.Subject = f
		s.ShellSubject = true
	}
}

// WithPaths sets the reporter of every path the tool touches; see Spec.Paths.
func WithPaths(f func(json.RawMessage) []permission.PathAccess) Option {
	return func(s *Spec) { s.Paths = f }
}

// Compile-time checks: built answers every optional interface but
// Dispatcher, which only builtDispatcher adds.
var (
	_ ReadOnlyer        = (*built)(nil)
	_ ConcurrencySafer  = (*built)(nil)
	_ PermissionChecker = (*built)(nil)
	_ Subjecter         = (*built)(nil)
	_ ShellSubjecter    = (*built)(nil)
	_ PathAccessor      = (*built)(nil)
	_ Editor            = (*built)(nil)
	_ RawInputer        = (*built)(nil)
	_ Availability      = (*built)(nil)
	_ Dispatcher        = (*builtDispatcher)(nil)
)

// ValidName reports why name is not a legal tool name: it must be non-empty
// and use only letters, digits and the characters "_.:-", which covers
// MCP-style names such as "mcp__srv__tool". Build panics on an invalid name;
// ValidName lets a caller that registers tools from untrusted input check
// first.
func ValidName(name string) error {
	if err := names.Validate(name); err != nil {
		return fmt.Errorf("tool: invalid name %q: %w", name, err)
	}
	return nil
}
