package permission

import (
	"fmt"
	"slices"
	"strings"
)

// Evaluate decides whether req may run under pc. It applies, in order, the
// first step that yields a verdict:
//
//  1. a matching deny rule denies
//  2. the tool's own deny verdict denies
//  3. a shell command the parser cannot vouch for asks (denies in dontAsk,
//     auto and plan modes) when deny or ask rules have patterns for its tool
//  4. bypass mode allows
//  5. plan mode denies anything not read-only
//  6. a matching ask rule asks
//  7. the tool's own allow verdict allows (not in dontAsk mode)
//  8. a matching allow rule allows
//  9. acceptEdits mode allows file edits, except writes to git metadata,
//     which ask (see gitMetadataWrite)
//  10. read-only calls are allowed
//  11. dontAsk and auto modes deny; every other mode asks
//
// Deny rules come first so that nothing, not even bypass mode, can override
// an explicit prohibition, and a tool's own refusal is likewise final, even
// in bypass mode: no mode or approval can override it. Ask rules come before
// the tool's own allow verdict, so a host's explicit request for approval is
// never waived by a tool that considers itself safe.
//
// For a shell subject (Request.Shell), a deny or ask rule with a pattern
// matches if any single command of the line matches, while allow rules must
// cover every command. Step 3 makes deny and ask rules over commands fail
// closed: a command line that may run something the rules cannot see needs
// a human, even in bypass mode.
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
	if m.unanalyzed(pc.Rules) {
		return unanalyzedDecision(req.Tool, pc.Mode)
	}
	// Step 4.
	if pc.Mode == ModeBypass {
		return Allowed()
	}
	// Step 5.
	if pc.Mode == ModePlan && !req.ReadOnly {
		return Denied("plan mode: read-only")
	}
	// Step 6.
	if rule, ok := m.first(pc.Rules.Ask); ok {
		return AskUser(fmt.Sprintf("approval required by rule %s", rule))
	}
	// Step 7.
	if req.SelfCheck.Behavior == Allow && pc.Mode != ModeDontAsk {
		return req.SelfCheck
	}
	// Step 8.
	if m.allowMatch(pc.Rules.Allow) {
		return Allowed()
	}
	// Step 9.
	if pc.Mode == ModeAcceptEdits && req.Edit {
		if p, ok := gitMetadataWrite(req.Paths); ok {
			return AskUser(fmt.Sprintf("%s writes git metadata (%s), where git config can name commands that git runs", req.Tool, p))
		}
		return Allowed()
	}
	// Step 10.
	if req.ReadOnly {
		return Allowed()
	}
	// Step 11.
	return fallback(req, pc.Mode)
}

// unanalyzedDecision refuses to pass a command the deny or ask rules could not be
// checked against: modes that never prompt (and plan mode, which runs
// nothing that changes state) deny it, the others ask a human.
func unanalyzedDecision(tool string, mode Mode) Decision {
	msg := fmt.Sprintf("command could not be fully analyzed against deny rules for %s", tool)
	if mode == ModeDontAsk || mode == ModeAuto || mode == ModePlan {
		return Denied(msg)
	}
	return AskUser(msg)
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

// unanalyzed reports whether the request is a shell command SplitCommand
// could not vouch for while deny or ask rules have patterns for its tool, so
// that matching its best-effort segments may have missed a command.
func (m matcher) unanalyzed(rules Rules) bool {
	if !m.req.Shell || m.vouched {
		return false
	}
	return slices.ContainsFunc(slices.Concat(rules.Deny, rules.Ask), func(r Rule) bool {
		return !r.isPathRule() && r.Tool == m.req.Tool && r.Pattern != ""
	})
}

// first returns the first deny or ask rule that matches the request. A tool
// rule's pattern matches a shell subject as a whole or any one of its
// commands, so "ls && rm x" cannot slip past a deny rule for rm. Commands
// also match ignoring case, since a case-insensitive file system runs rm
// for "RM x". Allow rules never fold case (see allowMatch).
func (m matcher) first(rules []Rule) (Rule, bool) {
	for _, r := range rules {
		folded := r
		folded.Pattern = strings.ToLower(r.Pattern)
		if m.matches(r) || slices.ContainsFunc(m.segments, func(seg string) bool {
			return folded.matchesCommand(m.req.Tool, strings.ToLower(seg))
		}) {
			return r, true
		}
	}
	return Rule{}, false
}

// matches reports whether r matches the request exactly, as a whole or, for
// a shell subject, any one of its commands.
func (m matcher) matches(r Rule) bool {
	return r.Matches(m.req) || slices.ContainsFunc(m.segments, func(seg string) bool {
		return r.matchesCommand(m.req.Tool, seg)
	})
}

// allowMatch reports whether allow rules permit the request. For a shell
// subject, a tool rule with a pattern never matches the line as plain text:
// the line must parse, and every one of its commands must match some rule
// (different rules may cover different commands), so "git log; rm x" is not
// allowed by execute(git:*). Other rules match as usual.
func (m matcher) allowMatch(rules []Rule) bool {
	if !m.req.Shell {
		return slices.ContainsFunc(rules, m.matches)
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
