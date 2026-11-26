package subagents

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Launcher builds subagent agents from a parent agent and prepares isolated
// runs exactly as the task tool does: the parent's Config with the spec's
// overrides, a State from Child, the stricter permission mode (childMode),
// the narrowed skills scope (childScope), the call's backend, Ephemeral. One
// agent per (parent, spec name) is cached; safe for concurrent use. The zero
// value is not usable; create one with NewLauncher.
//
// The caller owns the checks the task tool makes before launching: it must
// enforce the nesting depth (agent.RunInfo.Depth against its MaxDepth) and
// that spec is one it offers, and it must run the child under a context that
// keeps the call's values (context.WithoutCancel(ctx), not
// context.Background()), so that the child's Depth is the parent's plus one.
// Specs should come from ResolveSpecs.
type Launcher struct {
	// children maps a childKey to its *agent.Agent. Built subagents are
	// immutable agents rather than per-run data, so sharing them between
	// concurrent runs is safe.
	children sync.Map
	// extra is appended to every child's middleware.
	extra []agent.Middleware
}

// NewLauncher returns a Launcher that appends extra to every child's
// middleware (after the spec's or the inherited list). An extra that is
// already in that list as the very same value is not appended again; one
// whose name clashes with a different value in the list makes Agent fail.
// Identity needs a pointer or otherwise comparable middleware; a value that
// is not comparable is never "the same" and so clashes by name. NewLauncher
// does not check extra itself: two extras with the same name also make Agent
// fail, when agent.New rejects the duplicate.
func NewLauncher(extra ...agent.Middleware) *Launcher {
	return &Launcher{extra: slices.Clone(extra)}
}

// childKey identifies a built subagent: specs are fixed up front, but the
// configuration they inherit comes from the agent that delegates.
type childKey struct {
	parent *agent.Agent
	name   string
}

// Agent returns the agent for spec as started from parent, building it on
// first use. Agents are immutable and safe for concurrent runs, so one per
// (parent, subagent) serves every call. spec is validated like New does
// (see ResolveSpecs, which supplies ready specs). The child must be started
// with Input (or the task tool) for the stricter permission mode to apply:
// running it with another input starts it in its own configured mode.
func (l *Launcher) Agent(parent *agent.Agent, spec Subagent) (*agent.Agent, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	key := childKey{parent: parent, name: spec.Name}
	if cached, ok := l.children.Load(key); ok {
		return cached.(*agent.Agent), nil
	}
	cfg, err := childConfig(parent.Config(), spec)
	if err != nil {
		return nil, err
	}
	if cfg.Middleware, err = l.withExtra(cfg.Middleware); err != nil {
		return nil, fmt.Errorf("subagents: subagent %q: %w", spec.Name, err)
	}
	built, err := agent.New(cfg)
	if err != nil {
		return nil, err
	}
	// Two parallel first calls may both build; keep whichever won.
	actual, _ := l.children.LoadOrStore(key, built)
	return actual.(*agent.Agent), nil
}

// withExtra returns mws followed by the extra middleware it does not already
// hold, without modifying mws. An extra already present by identity keeps its
// position in mws; it is neither moved nor appended again.
func (l *Launcher) withExtra(mws []agent.Middleware) ([]agent.Middleware, error) {
	out := slices.Clone(mws)
	for _, x := range l.extra {
		if x == nil {
			return nil, errors.New("extra middleware is nil")
		}
		if slices.ContainsFunc(mws, func(m agent.Middleware) bool { return sameMiddleware(m, x) }) {
			continue
		}
		if slices.ContainsFunc(mws, func(m agent.Middleware) bool { return m.Name() == x.Name() }) {
			return nil, fmt.Errorf("extra middleware %q clashes with a middleware of that name", x.Name())
		}
		out = append(out, x)
	}
	return out, nil
}

// sameMiddleware reports whether a and b are the same value; values that are
// not comparable at run time (checked on the values, not just their types, so
// an interface field holding a slice-bearing value does not panic) are never
// the same.
func sameMiddleware(a, b agent.Middleware) bool {
	if a == nil || b == nil || !reflect.ValueOf(a).Comparable() || !reflect.ValueOf(b).Comparable() {
		return false
	}
	return a == b
}

// Input prepares a run of child, the agent Agent built for spec, on
// description alone, with the parent's shareable state and backend, under
// the thread id <parent thread>/<spec.Name>/<id>. ctx must belong to a run
// (agent.Info), else Input returns an error; that run is the parent. c must be
// the current tool call of that run (a nil c is an error); child and spec
// should come from Agent and ResolveSpecs. See childMode for the permission mode the
// child starts in and childScope for the skills it is offered.
func (l *Launcher) Input(ctx context.Context, child *agent.Agent, spec Subagent, description, id string, c *tool.Call) (agent.Input, error) {
	info, ok := agent.Info(ctx)
	if !ok {
		return agent.Input{}, errors.New("subagents: a subagent can only be started inside an agent run")
	}
	if c == nil || c.State == nil {
		return agent.Input{}, errors.New("subagents: Input needs the run's current tool call")
	}
	s := c.State.Child()
	if err := agent.SetPermissionMode(s, childMode(info.Agent, child, c.State)); err != nil {
		return agent.Input{}, err
	}
	// skills.ScopeKey is private, so Child dropped it: without this a
	// subagent (or its own subagents) would see skills its parent may not.
	if parent, ok := skills.ScopeKey.Get(c.State); ok || spec.Skills != nil || spec.PinnedSkills != nil {
		skills.ScopeKey.Set(s, childScope(parent, spec))
	}
	return agent.Input{
		ThreadID:  childThread(info.ThreadID, spec.Name, id),
		State:     s,
		Messages:  []llm.Message{llm.UserText(description)},
		Backend:   c.Backend,
		AgentName: spec.Name,
		// The parent's thread is the one that is saved.
		Ephemeral: true,
	}, nil
}

// childThread is the thread id of the subagent a call starts.
func childThread(parentThread, typ, toolUseID string) string {
	return parentThread + "/" + typ + "/" + toolUseID
}

// childMode is the permission mode child, an isolated subagent started by
// parent's run with State s, starts in: the stricter (permission.Stricter)
// of the parent's current mode and the child's configured one
// (Subagent.PermissionMode, else the parent's Config.PermissionMode). A
// subagent therefore never acts more freely than the run that started it,
// nor more freely than it was configured to.
func childMode(parent, child *agent.Agent, s *state.State) permission.Mode {
	return permission.Stricter(parent.PermissionMode(s), child.PermissionMode(nil))
}

// childConfig derives a subagent's configuration from its parent's. The
// subagent never checkpoints or syncs a workspace of its own: it runs inside
// the parent's thread and shares the parent's backend.
func childConfig(cfg agent.Config, spec Subagent) (agent.Config, error) {
	system, err := childSystem(spec)
	if err != nil {
		return agent.Config{}, err
	}
	cfg.Name = spec.Name
	cfg.System = system
	cfg.CacheBoundary = 1
	// The child never carries an OutputSchema: it always runs nested, where
	// the core field is inactive, and agent.New would refuse a spec schema
	// without "type": "object" that subagents accept. childSystem asks for
	// the spec's format and the task tool validates the answer.
	cfg.OutputSchema = nil
	cfg.Checkpointer = nil
	cfg.SyncWorkspace = false
	if spec.Tools != nil {
		cfg.Tools = spec.Tools
	}
	if spec.Middleware != nil {
		cfg.Middleware = spec.Middleware
	}
	if err := checkSkills(spec, cfg.Middleware); err != nil {
		return agent.Config{}, err
	}
	if spec.Provider != nil {
		cfg.Provider = spec.Provider
	}
	if spec.Model != "" {
		cfg.Model = spec.Model
		// The parent's profile was looked up for the parent's model.
		cfg.Profile = nil
	}
	if spec.PermissionMode != "" {
		cfg.PermissionMode = spec.PermissionMode
	}
	if spec.Rules != nil {
		cfg.Rules = *spec.Rules
	}
	if spec.MaxTurns != 0 {
		cfg.MaxTurns = spec.MaxTurns
	}
	return cfg, nil
}
