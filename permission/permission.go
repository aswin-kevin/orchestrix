// Package permission decides whether a tool call may run. It parses permission
// rules and evaluates them, together with the permission mode and the tool's
// own verdict, in a fixed order that is the SDK's safety boundary.
//
// It is a leaf package: it knows nothing about tools, backends or models.
//
// # Limits
//
// Rules are matched lexically. Path rules compare cleaned virtual paths,
// case-sensitively, and do not resolve symlinks: a rule for /secrets/** does
// not cover a link elsewhere that points into /secrets (the disk backend
// still refuses anything that resolves outside its root).
//
// Tool patterns match a plain subject, such as a URL, as text. A shell
// subject (Request.Shell) is split into its commands first (SplitCommand):
// an allow rule must cover every command, so execute(git:*) does not allow
// "git status && rm -rf x", while a deny or ask rule applies if it matches
// any one of them. A command line the parser cannot vouch for, such as one
// with command substitution, one that pipes into sh, one that sets a
// variable (PATH=..., LD_PRELOAD=...) or one that runs a program by path
// (./npm), is never allowed by a pattern rule. Deny rules over commands remain a best effort: a command
// can be reached in ways the text does not show (a script, an alias for
// another program, a wrapper the parser does not know). Permission rules
// decide which calls the agent makes; they are not a sandbox.
package permission

import (
	"context"
	"encoding/json"
	"slices"
)

// Behavior is the outcome of a permission check.
type Behavior string

// The possible behaviors.
const (
	Allow Behavior = "allow"
	Deny  Behavior = "deny"
	Ask   Behavior = "ask"
)

// Decision is a permission verdict. Message explains Deny and Ask. A tool's
// own Allow may carry UpdatedInput, a rewritten input to run instead.
type Decision struct {
	Behavior     Behavior
	Message      string
	UpdatedInput json.RawMessage
}

// Allowed returns an Allow decision.
func Allowed() Decision { return Decision{Behavior: Allow} }

// Denied returns a Deny decision with the given reason.
func Denied(msg string) Decision { return Decision{Behavior: Deny, Message: msg} }

// AskUser returns an Ask decision with the given prompt.
func AskUser(msg string) Decision { return Decision{Behavior: Ask, Message: msg} }

// Mode is the session-wide permission posture.
type Mode string

// The permission modes.
const (
	// ModeDefault asks about anything not covered by a rule or read-only.
	ModeDefault Mode = "default"
	// ModeAcceptEdits additionally allows file-modifying tools.
	ModeAcceptEdits Mode = "acceptEdits"
	// ModeBypass allows everything except what a deny rule forbids.
	ModeBypass Mode = "bypassPermissions"
	// ModePlan denies everything that is not read-only.
	ModePlan Mode = "plan"
	// ModeDontAsk never prompts: whatever is not explicitly allowed is denied,
	// and a tool's own allow verdict is not trusted.
	ModeDontAsk Mode = "dontAsk"
	// ModeAuto never prompts either, but honors a tool's own allow verdict.
	ModeAuto Mode = "auto"
)

// Modes returns every permission mode, in documentation order.
func Modes() []Mode {
	return []Mode{ModeDefault, ModeAcceptEdits, ModeBypass, ModePlan, ModeDontAsk, ModeAuto}
}

// Valid reports whether m is one of the defined modes. The empty Mode is not
// valid; configuration treats it as "unset" and applies ModeDefault rules.
func (m Mode) Valid() bool {
	return slices.Contains(Modes(), m)
}

// Op is the kind of access made to a path.
type Op string

// The path operations.
const (
	OpRead  Op = "read"
	OpWrite Op = "write"
)

// PathAccess is one filesystem access a tool call intends to make.
type PathAccess struct {
	// Path is a virtual absolute path. A relative path is treated as rooted at
	// the virtual root when rules are matched.
	Path string
	Op   Op
}

// Request describes a tool call to be checked.
type Request struct {
	// Tool is the tool's name; tool rules match it exactly.
	Tool  string
	Input json.RawMessage
	// ReadOnly marks a call that changes nothing.
	ReadOnly bool
	// Edit marks a file-modifying tool (relevant to ModeAcceptEdits).
	Edit bool
	// Subject is the primary string tool rule patterns match against, such as
	// a shell command or a URL.
	Subject string
	// Shell marks Subject as a shell command line: tool rules with a pattern
	// then match its individual commands (see SplitCommand). False means
	// patterns match Subject as plain text.
	Shell bool
	// Paths lists the filesystem accesses, matched by path rules.
	//
	// Every tool that touches the filesystem must report every path it
	// touches here; path rules cannot see accesses that are not listed.
	Paths []PathAccess
	// SelfCheck is the tool's own CheckPermission verdict.
	SelfCheck Decision
}

// Approver resolves an Ask decision, typically by prompting a human.
type Approver func(ctx context.Context, req Request, reason string) Decision

// Context carries the session's permission configuration.
type Context struct {
	Mode  Mode
	Rules Rules
}

// Rules groups permission rules by the behavior they trigger.
type Rules struct {
	Allow, Ask, Deny []Rule
}
