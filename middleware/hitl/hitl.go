// Package hitl adds human-in-the-loop approval to chosen tools, configured
// per tool name or name pattern: which decisions a human may answer with,
// and an optional predicate narrowing which calls need one.
//
// It sits on top of the permission pipeline and does not replace it: a
// configured call pauses even in bypass mode, but a deny rule still refuses
// it first. A call a human already approved in the permission step is not
// asked about again when its InterruptConfig allows Approve; when it does
// not, the approval does not count: the call is rejected when only Reject is
// allowed, and otherwise asked about again so that the human gives a decision
// hitl allows (an Edit or a Respond). An Approver can only allow, perhaps
// with a rewrite, or deny, so when the agent has one and Edit is not allowed
// either, the call is rejected without asking it. The pause itself is the
// agent's ordinary interrupt: the host answers it with Agent.Resume.
package hitl

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Name is the name of the hitl middleware.
const Name = "hitl"

// DefaultAllowed returns the decisions an InterruptConfig offers when its
// Allowed is nil: Approve, Edit and Reject. Each call returns a fresh copy.
func DefaultAllowed() []agent.DecisionType {
	return []agent.DecisionType{agent.Approve, agent.Edit, agent.Reject}
}

// approvedKey holds the tool_use ids hitl already cleared in this run. A tool
// resumed after that is answering an interrupt it raised itself, and must
// keep its own Resume value.
var approvedKey = state.NewKey[[]string]("hitl.approved", state.Private[[]string]())

// askedKey holds the tool_use ids hitl interrupted and has not yet seen
// answered, so a resumed call is recognized even when its edited input no
// longer satisfies When.
var askedKey = state.NewKey[[]string]("hitl.asked", state.Private[[]string]())

// InterruptConfig describes when and how one tool, or the tools matching a
// pattern, need a human decision. Where several pattern keys match one tool
// they are combined (see Config.InterruptOn).
type InterruptConfig struct {
	// Allowed lists the decisions the host may answer with: Approve, Edit,
	// Reject or Respond. Nil means DefaultAllowed(); an empty, non-nil list
	// means only Reject. An Approver is held to it too: its Allow runs the
	// call only when Approve is allowed, or when Edit is and it rewrites
	// the input; otherwise the call is rejected.
	Allowed []agent.DecisionType
	// When reports whether a call needs a decision, judging by its Input. Nil
	// means every call does.
	When func(ctx context.Context, call *agent.ToolCall) bool
	// Reason tells the human why a decision is needed. Empty means
	// "Approval required for <tool>".
	Reason string
}

// Config configures the middleware.
type Config struct {
	// InterruptOn maps a tool name pattern (tool.MatchName: * matches any
	// run of characters, ? exactly one) to its InterruptConfig. A key equal
	// to the tool's name wins outright. Otherwise every matching pattern
	// key applies, combined fail-closed so that a broad pattern never
	// weakens a narrower one: the call pauses when any of their When
	// functions says so (a nil When always does), Allowed is the
	// intersection of their Allowed sets (an unset one counts as
	// DefaultAllowed; an empty intersection allows only Reject), and the
	// Reason is the most specific key's (the most non-wildcard characters,
	// ties going to the key first in sorted order). An exact key replaces
	// the patterns, so it can be less strict than a pattern matching the
	// same tool. A tool no key matches is left alone; nil lists none.
	InterruptOn map[string]InterruptConfig
}

// Middleware is the hitl middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type Middleware struct {
	// on is a copy of Config.InterruptOn.
	on map[string]InterruptConfig
	// keys are the keys of on, sorted.
	keys []string
}

// New validates cfg and returns the hitl middleware. It fails for an
// InterruptOn key that is empty, blank or holds a NUL byte
// (tool.ValidPattern), and for an Allowed decision other than Approve, Edit,
// Reject and Respond. cfg is copied: changing it afterwards has no effect.
func New(cfg Config) (*Middleware, error) {
	on := make(map[string]InterruptConfig, len(cfg.InterruptOn))
	for key, ic := range cfg.InterruptOn {
		if err := tool.ValidPattern(key); err != nil {
			return nil, fmt.Errorf("hitl: InterruptOn key %q: %w", key, err)
		}
		for _, d := range ic.Allowed {
			if !slices.Contains(knownDecisions, d) {
				return nil, fmt.Errorf("hitl: InterruptOn[%q] allows the unknown decision %q", key, d)
			}
		}
		switch {
		case ic.Allowed == nil:
		case len(ic.Allowed) == 0:
			ic.Allowed = []agent.DecisionType{agent.Reject}
		default:
			ic.Allowed = slices.Clone(ic.Allowed)
		}
		on[key] = ic
	}
	return &Middleware{on: on, keys: slices.Sorted(maps.Keys(on))}, nil
}

// knownDecisions are the decisions an InterruptConfig may allow.
var knownDecisions = []agent.DecisionType{agent.Approve, agent.Edit, agent.Reject, agent.Respond}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Required implements agent.Required: approvals are a safety check, so a
// MiddlewareExcluder may not remove them.
func (m *Middleware) Required() bool { return true }

// configFor returns the InterruptConfig for the tool called name. A key
// equal to name wins outright. Otherwise every matching pattern key is
// combined fail-closed (see Config.InterruptOn). ok is false when no key
// matches.
func (m *Middleware) configFor(name string) (cfg InterruptConfig, ok bool) {
	if cfg, ok := m.on[name]; ok {
		return cfg, true
	}
	var matched []InterruptConfig
	reason, best := "", -1
	for _, key := range m.keys {
		if !tool.MatchName(key, name) {
			continue
		}
		c := m.on[key]
		matched = append(matched, c)
		// Keys are sorted, so a tie keeps the earlier key.
		if n := literals(key); n > best {
			reason, best = c.Reason, n
		}
	}
	switch len(matched) {
	case 0:
		return InterruptConfig{}, false
	case 1:
		return matched[0], true
	}
	return InterruptConfig{Allowed: intersect(matched), When: anyWhen(matched), Reason: reason}, true
}

// literals counts the characters of pattern that are not wildcards.
func literals(pattern string) int {
	n := 0
	for _, r := range pattern {
		if r != '*' && r != '?' {
			n++
		}
	}
	return n
}

// intersect returns the decisions every config allows, in the order of the
// first; an unset Allowed is DefaultAllowed. An empty intersection allows
// only Reject.
func intersect(cfgs []InterruptConfig) []agent.DecisionType {
	out := slices.Clone(allowedOf(cfgs[0]))
	for _, c := range cfgs[1:] {
		allowed := allowedOf(c)
		out = slices.DeleteFunc(out, func(d agent.DecisionType) bool { return !slices.Contains(allowed, d) })
	}
	if len(out) == 0 {
		return []agent.DecisionType{agent.Reject}
	}
	return out
}

// anyWhen returns a When that pauses when any config's When does; nil (every
// call pauses) when one of them has no When.
func anyWhen(cfgs []InterruptConfig) func(context.Context, *agent.ToolCall) bool {
	whens := make([]func(context.Context, *agent.ToolCall) bool, 0, len(cfgs))
	for _, c := range cfgs {
		if c.When == nil {
			return nil
		}
		whens = append(whens, c.When)
	}
	return func(ctx context.Context, call *agent.ToolCall) bool {
		return slices.ContainsFunc(whens, func(when func(context.Context, *agent.ToolCall) bool) bool { return when(ctx, call) })
	}
}

// WrapTool implements agent.ToolWrapper.
//
// A configured Approver may be called concurrently for parallel tool calls,
// and the call holds its concurrency slot while it waits, so it must be safe
// for concurrent use and should not block indefinitely.
func (m *Middleware) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	cfg, ok := m.configFor(call.Name)
	switch {
	case !ok || cleared(call):
		return next(ctx, call)
	case call.Call.Resume != nil && asked(call):
		// Answering hitl's own interrupt: When judged the original input,
		// and an edit may have changed what it says.
		return resumed(ctx, call, next)
	case cfg.When != nil && !cfg.When(ctx, call):
		return next(ctx, call)
	case call.Approved && slices.Contains(allowedOf(cfg), agent.Approve):
		// The permission step already asked a human, and a plain approval
		// is a decision hitl allows too.
		return next(ctx, call)
	case call.Approved && !slices.ContainsFunc(allowedOf(cfg), notReject):
		// Only Reject is allowed: no human decision could run the call.
		return rejected("approval is not an allowed decision for " + call.Name), nil
	}
	// A call approved in the permission step whose config does not allow a
	// plain Approve still needs a decision hitl allows (an Edit or a
	// Respond), so it is asked again below; an Approver is asked only when
	// Edit is allowed, the one such decision it can give.
	if a, approver := approverOf(ctx); approver != nil {
		if call.Approved && !slices.Contains(allowedOf(cfg), agent.Edit) {
			return rejected("approval is not an allowed decision for " + call.Name), nil
		}
		return ask(ctx, a, approver, cfg, call, next)
	}
	askedKey.Update(call.State, func(ids []string) []string { return append(ids, call.ID) })
	return tool.Result{}, interruptFor(cfg, call.Name)
}

// notReject reports whether d is a decision other than Reject.
func notReject(d agent.DecisionType) bool { return d != agent.Reject }

// cleared reports whether hitl already let this call through.
func cleared(call *agent.ToolCall) bool {
	ids, _ := approvedKey.Get(call.State)
	return slices.Contains(ids, call.ID)
}

// asked reports whether hitl raised an interrupt for this call that is still
// unanswered.
func asked(call *agent.ToolCall) bool {
	ids, _ := askedKey.Get(call.State)
	return slices.Contains(ids, call.ID)
}

// resumed carries out the human's decision on hitl's own interrupt. The loop
// has already applied an edit's new input, schema-checked it and checked it
// against the permission rules again.
func resumed(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	resume := call.Call.Resume
	call.Call.Resume = nil // consumed: the tool must not mistake it for its own
	askedKey.Update(call.State, func(ids []string) []string { return without(ids, call.ID) })
	if resume.Type == tool.ResumeRespond {
		return tool.Text(resume.Message), nil
	}
	approvedKey.Update(call.State, func(ids []string) []string { return append(ids, call.ID) })
	return next(ctx, call)
}

// without returns ids minus id, without touching ids.
func without(ids []string, id string) []string {
	return slices.DeleteFunc(slices.Clone(ids), func(s string) bool { return s == id })
}

// ask puts the question to approver, the Approver of agent a running the
// call, and waits for the answer. The question describes the call as the
// permission step does (paths, subject, edit); building it runs the tool's
// own CheckPermission once more, for SelfCheck. A rewrite of the input is
// honored only where the host may edit, and is checked against the
// permission rules like any edited call. An Allow runs the call only when
// Approve is allowed, or Edit is and the Approver rewrote the input.
func ask(ctx context.Context, a *agent.Agent, approver permission.Approver, cfg InterruptConfig, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	decision := approver(ctx, a.PermissionRequest(ctx, call), reasonFor(cfg, call.Name))
	if decision.Behavior != permission.Allow {
		return rejected(decision.Message), nil
	}
	allowed := allowedOf(cfg)
	edit := len(decision.UpdatedInput) > 0 && slices.Contains(allowed, agent.Edit)
	if !edit && !slices.Contains(allowed, agent.Approve) {
		// The Approver is not told Allowed: an Allow it may not give is
		// a refusal.
		return rejected("approval is not an allowed decision for " + call.Name), nil
	}
	if edit {
		if res := applyRewrite(ctx, a, call, decision.UpdatedInput); res != nil {
			return *res, nil
		}
	}
	return next(ctx, call)
}

// applyRewrite makes in the call's input after checking it against the
// tool's schema and the permission rules. It returns an error result when the
// rewrite is refused. The Approver's allow stands in for an Ask, but a deny
// rule (or the mode) of agent a still wins.
func applyRewrite(ctx context.Context, a *agent.Agent, call *agent.ToolCall, in json.RawMessage) *tool.Result {
	if err := tool.CheckInput(call.Tool, in); err != nil {
		res := tool.Errorf("invalid input for %s: %v", call.Name, err)
		return &res
	}
	edited := *call
	edited.Input = in
	if d := a.CheckPermission(ctx, &edited); d.Behavior == permission.Deny {
		res := tool.Errorf("Permission denied: %s", d.Message)
		return &res
	}
	call.Input = in
	return nil
}

// rejected is the result of a call the Approver refused.
func rejected(message string) tool.Result {
	if message == "" {
		return tool.Errorf("Rejected.")
	}
	return tool.Errorf("Rejected: %s", message)
}

// approverOf returns the agent running ctx and its Approver; the Approver
// is nil when the agent has none or ctx is not inside a run.
func approverOf(ctx context.Context) (*agent.Agent, permission.Approver) {
	info, ok := agent.Info(ctx)
	if !ok {
		return nil, nil
	}
	return info.Agent, info.Agent.Config().Approver
}

// interruptFor builds the interrupt that asks the host about a call of name.
func interruptFor(cfg InterruptConfig, name string) *agent.Interrupt {
	return &agent.Interrupt{Requests: []agent.ActionRequest{{
		Reason:  reasonFor(cfg, name),
		Allowed: slices.Clone(allowedOf(cfg)),
	}}}
}

// allowedOf returns the decisions cfg offers.
func allowedOf(cfg InterruptConfig) []agent.DecisionType {
	if cfg.Allowed == nil {
		return DefaultAllowed()
	}
	return cfg.Allowed
}

func reasonFor(cfg InterruptConfig, name string) string {
	if cfg.Reason != "" {
		return cfg.Reason
	}
	return "Approval required for " + name
}
