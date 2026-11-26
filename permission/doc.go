// Package permission decides whether a tool call may run. It parses permission
// rules and evaluates them, together with the permission mode and the tool's
// own verdict, in a fixed order that is the SDK's safety boundary.
//
// It is a leaf package: it knows nothing about tools, backends or models.
//
// Mode and the rule grammar are closed sets on purpose: Stricter depends on
// a total order and they are the safety boundary. Custom policy belongs in
// a tool's CheckPermission, a ToolWrapper, or an Approver.
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
// (./npm), is never allowed by a pattern rule. Deny and ask rules over
// commands are enforced fail closed: a command line the parser cannot vouch
// for, when such rules exist for its tool, needs a human (or is denied in
// modes that never prompt), even in bypass mode. Rules still name programs,
// not behaviors: a vouched-for command such as a script or a program that
// runs others can reach anything. Permission rules decide which calls the
// agent makes; they are not a sandbox.
package permission
