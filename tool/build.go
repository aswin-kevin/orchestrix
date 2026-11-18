package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aswin-kevin/orchestrix/internal/names"
	"github.com/aswin-kevin/orchestrix/permission"
)

// Spec describes a tool declaratively. Every optional field defaults to the
// most restrictive behaviour, so forgetting one never widens what a tool may do.
type Spec struct {
	// Name must be non-empty and use only [A-Za-z0-9_.:-].
	Name        string
	Description string
	// Schema is the input schema; nil means a tool without parameters.
	Schema map[string]any

	// ReadOnly, Concurrent and Edit default to false when nil.
	ReadOnly   func(json.RawMessage) bool
	Concurrent func(json.RawMessage) bool
	Edit       func(json.RawMessage) bool
	// Subject returns the string permission rules match against (a shell
	// command, a URL); nil means none.
	Subject func(json.RawMessage) string
	// Paths must report every file-system path the call touches, as virtual
	// absolute paths, because permission checks see nothing else. Nil means
	// the tool touches no paths.
	Paths func(json.RawMessage) []permission.PathAccess
	// Permission is the tool's own verdict; nil asks the user.
	Permission func(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision
	// Run executes the tool and is required.
	Run func(ctx context.Context, in json.RawMessage, c *Call) (Result, error)
	// RawInput marks the tool as accepting input that bypasses validation.
	RawInput bool
}

// Build turns a Spec into a Tool. It panics on an invalid name or a missing Run,
// which are programming errors best caught at start-up.
func Build(s Spec) Tool {
	if err := names.Validate(s.Name); err != nil {
		panic(fmt.Sprintf("tool.Build: %v", err))
	}
	if s.Run == nil {
		panic(fmt.Sprintf("tool.Build: tool %q has no Run function", s.Name))
	}
	if s.Schema == nil {
		s.Schema = emptyObjectSchema()
	}
	return &built{spec: s}
}

// built adapts a Spec to Tool and to every optional interface; absent funcs
// answer with the fail-closed default.
type built struct{ spec Spec }

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

func (b *built) Paths(in json.RawMessage) []permission.PathAccess {
	if b.spec.Paths == nil {
		return nil
	}
	return b.spec.Paths(in)
}

func (b *built) CheckPermission(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision {
	if b.spec.Permission == nil {
		return permission.AskUser(fmt.Sprintf("approve %s?", b.spec.Name))
	}
	return b.spec.Permission(ctx, in, pc)
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

// WithPaths sets the reporter of every path the tool touches; see Spec.Paths.
func WithPaths(f func(json.RawMessage) []permission.PathAccess) Option {
	return func(s *Spec) { s.Paths = f }
}
