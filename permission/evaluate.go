package permission

import (
	"fmt"
	"slices"
)

// Evaluate decides whether req may run under pc. It applies, in order, the
// first step that yields a verdict:
//
//  1. a matching deny rule denies
//  2. the tool's own deny verdict denies
//  3. bypass mode allows
//  4. plan mode denies anything not read-only
//  5. a matching ask rule asks
//  6. the tool's own allow verdict allows (not in dontAsk mode)
//  7. a matching allow rule allows
//  8. acceptEdits mode allows file edits
//  9. read-only calls are allowed
//  10. dontAsk and auto modes deny; every other mode asks
//
// Deny rules come first so that nothing, not even bypass mode, can override
// an explicit prohibition, and a tool's own refusal is likewise final, even
// in bypass mode: no mode or approval can override it. Ask rules come before
// the tool's own allow verdict, so a host's explicit request for approval is
// never waived by a tool that considers itself safe.
//
// For a shell subject (Request.Shell), a deny or ask rule with a pattern
// matches if any single command of the line matches, while allow rules must
// cover every command.
func Evaluate(req Request, pc Context) Decision {
	m := newMatcher(req)
	// Step 1.
	if rule, ok := m.first(pc.Rules.Deny); ok {
		return Denied(fmt.Sprintf("denied by rule %s", rule))
	}
	// Step 2.
	if req.SelfCheck.Behavior == Deny {
		return Denied(req.SelfCheck.Message)
	}
	// Step 3.
	if pc.Mode == ModeBypass {
		return Allowed()
	}
	// Step 4.
	if pc.Mode == ModePlan && !req.ReadOnly {
		return Denied("plan mode: read-only")
	}
	// Step 5.
	if rule, ok := m.first(pc.Rules.Ask); ok {
		return AskUser(fmt.Sprintf("approval required by rule %s", rule))
	}
	// Step 6.
	if req.SelfCheck.Behavior == Allow && pc.Mode != ModeDontAsk {
		return req.SelfCheck
	}
	// Step 7.
	if m.allowMatch(pc.Rules.Allow) {
		return Allowed()
	}
	// Step 8.
	if pc.Mode == ModeAcceptEdits && req.Edit {
		return Allowed()
	}
	// Step 9.
	if req.ReadOnly {
		return Allowed()
	}
	// Step 10.
	return fallback(req, pc.Mode)
}

// fallback handles a call that no earlier step settled: non-interactive modes
// refuse it, interactive ones ask.
func fallback(req Request, mode Mode) Decision {
	if mode == ModeDontAsk || mode == ModeAuto {
		return Denied(fmt.Sprintf("%s is not permitted in %s mode", req.Tool, mode))
	}
	if req.SelfCheck.Behavior == Ask && req.SelfCheck.Message != "" {
		return AskUser(req.SelfCheck.Message)
	}
	return AskUser(fmt.Sprintf("approve %s?", req.Tool))
}

// matcher matches rules against one request, splitting a shell subject
// once for all of them.
type matcher struct {
	req Request
	// segments are the commands of a shell subject (nil otherwise), a best
	// effort even when SplitCommand could not vouch for them.
	segments []string
	// vouched reports that segments are every command the subject runs.
	vouched bool
}

func newMatcher(req Request) matcher {
	m := matcher{req: req}
	if req.Shell {
		m.segments, m.vouched = SplitCommand(req.Subject)
	}
	return m
}

// first returns the first rule that matches the request. A tool rule's
// pattern matches a shell subject as a whole or any one of its commands, so
// "ls && rm x" cannot slip past a deny rule for rm.
func (m matcher) first(rules []Rule) (Rule, bool) {
	for _, r := range rules {
		if r.Matches(m.req) || slices.ContainsFunc(m.segments, func(seg string) bool {
			return r.matchesCommand(m.req.Tool, seg)
		}) {
			return r, true
		}
	}
	return Rule{}, false
}

// allowMatch reports whether allow rules permit the request. For a shell
// subject, a tool rule with a pattern never matches the line as plain text:
// the line must parse, and every one of its commands must match some rule
// (different rules may cover different commands), so "git log; rm x" is not
// allowed by execute(git:*). Other rules match as usual.
func (m matcher) allowMatch(rules []Rule) bool {
	if !m.req.Shell {
		_, ok := m.first(rules)
		return ok
	}
	var patterned []Rule
	for _, r := range rules {
		if r.isPathRule() || r.Pattern == "" {
			if r.Matches(m.req) {
				return true
			}
			continue
		}
		patterned = append(patterned, r)
	}
	if !m.vouched {
		return false
	}
	for _, seg := range m.segments {
		if !slices.ContainsFunc(patterned, func(r Rule) bool { return r.matchesCommand(m.req.Tool, seg) }) {
			return false
		}
	}
	return true
}
