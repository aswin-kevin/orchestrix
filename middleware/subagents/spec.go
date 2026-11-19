package subagents

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/names"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Subagent describes one kind of subagent the task tool can start. Every
// field left at its zero value is inherited from the agent whose run calls
// the task tool.
type Subagent struct {
	// Name is the subagent_type the model selects it by, and the name its
	// events carry. Required; it must be a legal tool name (letters, digits
	// and _.:-), and Fork is reserved.
	Name string
	// Description tells the parent model when to use this subagent.
	// Required.
	Description string
	// SystemPrompt is the subagent's whole system prompt; it replaces the
	// parent's. Required, except for a spec named GeneralPurpose, where empty
	// means Config.GeneralPurposePrompt.
	SystemPrompt string
	// Tools are the subagent's tools; nil means the parent agent's
	// Config.Tools. Tools contributed by middleware come with Middleware.
	Tools []tool.Tool
	// Middleware is the subagent's user middleware; nil means the parent
	// agent's Config.Middleware, this middleware included, so the subagent
	// can delegate further up to Config.MaxDepth.
	Middleware []agent.Middleware
	// Provider and Model select the subagent's model; nil and "" mean the
	// parent's. Setting Model also re-derives the model profile from it.
	Provider llm.Provider
	Model    string
	// PermissionMode is the subagent's configured permission mode; "" means
	// the parent's Config.PermissionMode. A run starts in the stricter of it
	// and the parent's current mode (see the package documentation).
	PermissionMode permission.Mode
	// Rules replaces the parent's permission rules; nil means the parent's.
	Rules *permission.Rules
	// MaxTurns bounds the subagent's run; 0 means the parent's MaxTurns.
	MaxTurns int
	// Skills limits the skills offered to the subagent (see
	// skills.Scope) to these names, within those the delegating run is
	// offered itself; nil offers the delegating run's skills, so a
	// subagent never sees more skills than the run that started it. Each
	// name must be defined by the skills middleware in the subagent's
	// stack.
	Skills []string
	// PinnedSkills are skills whose instructions are injected into the
	// subagent's prompt, whether or not they are offered; nil keeps the
	// delegating run's scope-pinned skills. Each name must be defined by
	// the skills middleware in the subagent's stack.
	PinnedSkills []string
	// OutputSchema, when non-nil, is the JSON schema the subagent's final
	// answer must satisfy; the answer must be a JSON object. An invalid
	// answer is sent back for correction up to DefaultOutputRetries times;
	// the count starts over when a suspended subagent is resumed. Nil means
	// the final text is returned as is.
	OutputSchema map[string]any
}

// validateSpecs checks the configured subagents and returns them keyed by
// name, with the built-in general-purpose subagent added unless disabled or
// replaced, together with the names in the order the model sees them.
func validateSpecs(cfg Config, generalPrompt string) (map[string]Subagent, []string, error) {
	specs := make(map[string]Subagent, len(cfg.Subagents)+1)
	var order []string
	add := func(s Subagent) {
		specs[s.Name] = s
		order = append(order, s.Name)
	}
	if !cfg.DisableGeneralPurpose && !hasSpec(cfg.Subagents, GeneralPurpose) {
		add(Subagent{Name: GeneralPurpose, Description: generalPurposeDescription, SystemPrompt: generalPrompt})
	}
	for _, s := range cfg.Subagents {
		if s.Name == GeneralPurpose && s.SystemPrompt == "" {
			s.SystemPrompt = generalPrompt
		}
		if err := validateSpec(s); err != nil {
			return nil, nil, err
		}
		if _, dup := specs[s.Name]; dup {
			return nil, nil, fmt.Errorf("subagents: subagent %q is configured twice", s.Name)
		}
		add(s)
	}
	if len(order) == 0 {
		return nil, nil, errors.New("subagents: no subagents configured")
	}
	return specs, order, nil
}

// validateSpec checks the required fields of one subagent.
func validateSpec(s Subagent) error {
	if err := names.Validate(s.Name); err != nil {
		return fmt.Errorf("subagents: subagent %q: %w", s.Name, err)
	}
	switch {
	case s.Name == Fork:
		return fmt.Errorf("subagents: the subagent name %q is reserved", Fork)
	case s.Description == "":
		return fmt.Errorf("subagents: subagent %q: Description is required", s.Name)
	case s.SystemPrompt == "":
		return fmt.Errorf("subagents: subagent %q: SystemPrompt is required", s.Name)
	}
	if _, err := childSystem(s); err != nil {
		return fmt.Errorf("subagents: subagent %q: %w", s.Name, err)
	}
	// A subagent inheriting the parent's middleware is checked when it is
	// built (see childConfig), or earlier by a host calling CheckSkills.
	if s.Middleware != nil {
		return s.CheckSkills(nil)
	}
	return nil
}

// CheckSkills reports a name in s.Skills or s.PinnedSkills that the skills
// middleware of the subagent's stack does not define, or the lack of a skills
// middleware when either is set. That stack is s.Middleware when set, else
// inherited: the Config.Middleware of the agent that delegates. New checks
// the specs that set Middleware; the others are checked when the subagent is
// first built, unless the host that assembles the delegating agent calls
// CheckSkills with its stack first, as the orchestrix preset does.
func (s Subagent) CheckSkills(inherited []agent.Middleware) error {
	stack := inherited
	if s.Middleware != nil {
		stack = s.Middleware
	}
	if err := checkSkills(s, stack); err != nil {
		return fmt.Errorf("subagents: subagent %q: %w", s.Name, err)
	}
	return nil
}

// checkSkills reports a skill named by spec.Skills or spec.PinnedSkills that
// the skills middleware among mws, the subagent's stack, does not define.
func checkSkills(spec Subagent, mws []agent.Middleware) error {
	wanted := slices.Concat(spec.Skills, spec.PinnedSkills)
	if len(wanted) == 0 {
		return nil
	}
	var sk *skills.Middleware
	for _, mw := range mws {
		if found, ok := mw.(*skills.Middleware); ok {
			sk = found
		}
	}
	if sk == nil {
		return errors.New("Skills and PinnedSkills need a skills middleware in the subagent's stack")
	}
	for _, name := range wanted {
		if !sk.Has(name) {
			return fmt.Errorf("skill %q is not defined", name)
		}
	}
	return nil
}

// childScope is the skills scope of a subagent spec started by a run whose
// scope is parent: the parent's scope narrowed to spec.Skills when set, with
// spec.PinnedSkills replacing the pinned skills when set.
func childScope(parent skills.Scope, spec Subagent) skills.Scope {
	scope := skills.Scope{Restrict: parent.Restrict, Allowed: slices.Clone(parent.Allowed), Pinned: slices.Clone(parent.Pinned)}
	if spec.Skills != nil {
		allowed := slices.Clone(spec.Skills)
		if parent.Restrict {
			allowed = slices.DeleteFunc(allowed, func(name string) bool { return !slices.Contains(parent.Allowed, name) })
		}
		scope.Restrict, scope.Allowed = true, allowed
	}
	if spec.PinnedSkills != nil {
		scope.Pinned = slices.Clone(spec.PinnedSkills)
	}
	return scope
}

func hasSpec(specs []Subagent, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// childKey identifies a built subagent: specs are fixed at New, but the
// configuration they inherit comes from the agent that delegates.
type childKey struct {
	parent *agent.Agent
	name   string
}

// child returns the agent for spec as started from parent, building it on
// first use. Agents are immutable and safe for concurrent runs, so one per
// (parent, subagent) serves every task call.
func (m *middleware) child(parent *agent.Agent, spec Subagent) (*agent.Agent, error) {
	key := childKey{parent: parent, name: spec.Name}
	if cached, ok := m.children.Load(key); ok {
		return cached.(*agent.Agent), nil
	}
	cfg, err := childConfig(parent.Config(), spec)
	if err != nil {
		return nil, err
	}
	built, err := agent.New(cfg)
	if err != nil {
		return nil, err
	}
	// Two parallel first calls may both build; keep whichever won.
	actual, _ := m.children.LoadOrStore(key, built)
	return actual.(*agent.Agent), nil
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
	if spec.MaxTurns > 0 {
		cfg.MaxTurns = spec.MaxTurns
	}
	return cfg, nil
}

// childSystem is the subagent's system prompt, followed by the required
// answer format when it has an OutputSchema.
func childSystem(spec Subagent) ([]string, error) {
	if spec.OutputSchema == nil {
		return []string{spec.SystemPrompt}, nil
	}
	schema, err := json.Marshal(spec.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("encode OutputSchema: %w", err)
	}
	return []string{spec.SystemPrompt, outputFormatPrompt + string(schema)}, nil
}
