package permission

import "fmt"

// Evaluate decides whether req may run under pc. It applies, in order, the
// first step that yields a verdict:
//
//  1. a matching deny rule denies
//  2. bypass mode allows
//  3. plan mode denies anything not read-only
//  4. the tool's own deny verdict denies
//  5. the tool's own allow verdict allows (not in dontAsk mode)
//  6. a matching ask rule asks
//  7. a matching allow rule allows
//  8. acceptEdits mode allows file edits
//  9. read-only calls are allowed
//  10. dontAsk and auto modes deny; every other mode asks
//
// Deny rules come first so that nothing, not even bypass mode, can override
// an explicit prohibition.
func Evaluate(req Request, pc Context) Decision {
	// Step 1.
	if rule, ok := firstMatch(pc.Rules.Deny, req); ok {
		return Denied(fmt.Sprintf("denied by rule %s", rule))
	}
	// Step 2.
	if pc.Mode == ModeBypass {
		return Allowed()
	}
	// Step 3.
	if pc.Mode == ModePlan && !req.ReadOnly {
		return Denied("plan mode: read-only")
	}
	// Step 4.
	if req.SelfCheck.Behavior == Deny {
		return Denied(req.SelfCheck.Message)
	}
	// Step 5.
	if req.SelfCheck.Behavior == Allow && pc.Mode != ModeDontAsk {
		return req.SelfCheck
	}
	// Step 6.
	if rule, ok := firstMatch(pc.Rules.Ask, req); ok {
		return AskUser(fmt.Sprintf("approval required by rule %s", rule))
	}
	// Step 7.
	if _, ok := firstMatch(pc.Rules.Allow, req); ok {
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

// firstMatch returns the first rule that matches req.
func firstMatch(rules []Rule, req Request) (Rule, bool) {
	for _, r := range rules {
		if r.Matches(req) {
			return r, true
		}
	}
	return Rule{}, false
}
