package skills

import (
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Limits and names of the skill format.
const (
	// SkillFile is the file that defines a skill inside its directory.
	SkillFile = "SKILL.md"
	// MaxNameLen is the longest legal skill name.
	MaxNameLen = 64
	// MaxDescriptionLen is the longest legal description, in bytes.
	MaxDescriptionLen = 1024
	// MaxCompatibilityLen is the longest legal compatibility note, in bytes.
	MaxCompatibilityLen = 500
	// MaxBodyBytes caps a skill's body when it is loaded.
	MaxBodyBytes = 256 << 10
	// MaxFileBytes caps a skill file read through the skill tool.
	MaxFileBytes = 256 << 10
	// MaxListedFiles caps Skill.Files.
	MaxListedFiles = 50
	// ToolSkill is the name of the tool that loads a skill.
	ToolSkill = "skill"
)

// Name is the skills middleware's name (agent.Middleware.Name). The
// subagents middleware looks up the middleware of this name in a subagent's
// stack and asks it, as a Definer, whether the skills the subagent names
// exist.
const Name = "skills"

// Definer is what subagents need from the skills middleware: whether a
// skill exists. A replacement skills middleware (one named Name) implements
// it, and must also honour ScopeKey: the subagents middleware narrows a
// subagent's skills (Subagent.Skills and PinnedSkills) only by writing the
// child run's Scope there, so a replacement that ignores it offers the
// subagent every skill.
type Definer interface {
	// Has reports whether a skill named name is defined.
	Has(name string) bool
}

// Skill is one loaded skill.
type Skill struct {
	// Name identifies the skill; it follows the rules of Validate.
	Name string
	// Description says what the skill does and when to use it. Required.
	Description string
	// License and Compatibility are optional free text.
	License string
	// Compatibility is optional free text on the environments the skill
	// suits; License is optional free text naming its license.
	Compatibility string
	// Metadata holds the frontmatter's extra key/value pairs; nil if none.
	Metadata map[string]string
	// AllowedTools are the tool names the skill unlocks once loaded
	// (frontmatter "allowed-tools": a space-separated string or a list).
	AllowedTools []string
	// MCPServers are the MCP servers the skill asks for (frontmatter
	// "mcp-servers").
	MCPServers []string
	// Body is the Markdown after the frontmatter, capped at
	// MaxBodyBytes.
	Body string
	// FS and Dir locate the skill directory for file reads; both are zero for
	// a programmatic skill, which has no files.
	FS fs.FS
	// Dir is the skill's directory within FS.
	Dir string
	// Files lists the skill's other regular files as slash paths relative to
	// Dir, sorted, at most MaxListedFiles; nil if none.
	Files []string
}

// Validate reports why s is not a legal skill: the name must be 1 to
// MaxNameLen characters of [a-z0-9-] with no leading, trailing or doubled
// hyphen and, unless dirName is empty, equal to dirName; the description must
// be non-empty and at most MaxDescriptionLen bytes; the compatibility note at
// most MaxCompatibilityLen bytes; and no allowed-tools pattern may be made
// only of the wildcards "*" and "?" (such as "*" or "**"), which would unlock
// every gated tool. A broad prefix such as "mcp__*" is legal but unlocks
// every tool it matches, here every MCP server's. The error starts with
// "skills: ".
func Validate(s Skill, dirName string) error {
	if err := validate(s, dirName); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	return nil
}

// validate is Validate without the package prefix, for callers that wrap
// the error with their own.
func validate(s Skill, dirName string) error {
	if err := validateName(s.Name); err != nil {
		return err
	}
	if dirName != "" && s.Name != dirName {
		return fmt.Errorf("name %q does not match its directory %q", s.Name, dirName)
	}
	if strings.TrimSpace(s.Description) == "" {
		return fmt.Errorf("description is required")
	}
	if len(s.Description) > MaxDescriptionLen {
		return fmt.Errorf("description is %d bytes, over the limit of %d", len(s.Description), MaxDescriptionLen)
	}
	if len(s.Compatibility) > MaxCompatibilityLen {
		return fmt.Errorf("compatibility is %d bytes, over the limit of %d", len(s.Compatibility), MaxCompatibilityLen)
	}
	if i := slices.IndexFunc(s.AllowedTools, wildcardsOnly); i >= 0 {
		return fmt.Errorf("allowed-tools pattern %q is only wildcards, which would unlock every tool; name the tools", s.AllowedTools[i])
	}
	return nil
}

// wildcardsOnly reports whether pattern is made only of "*" and "?".
func wildcardsOnly(pattern string) bool {
	return pattern != "" && strings.Trim(pattern, "*?") == ""
}

// clone returns s with its own copies of the slices and the metadata map.
func (s Skill) clone() Skill {
	s.Metadata = maps.Clone(s.Metadata)
	s.AllowedTools = slices.Clone(s.AllowedTools)
	s.MCPServers = slices.Clone(s.MCPServers)
	s.Files = slices.Clone(s.Files)
	return s
}

func validateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("name is required")
	case utf8.RuneCountInString(name) > MaxNameLen:
		return fmt.Errorf("name is longer than %d characters", MaxNameLen)
	case strings.ContainsFunc(name, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') }):
		return fmt.Errorf("name %q must use only a-z, 0-9 and -", name)
	case strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-"):
		return fmt.Errorf("name %q must not start or end with a hyphen", name)
	case strings.Contains(name, "--"):
		return fmt.Errorf("name %q must not contain consecutive hyphens", name)
	}
	return nil
}

// Scope narrows what a run is offered. The zero value offers everything and
// pins nothing extra.
type Scope struct {
	// Restrict limits the catalog to the names in Allowed; false offers every
	// skill. Restrict with an empty Allowed offers none.
	Restrict bool `json:"restrict,omitempty"`
	// Allowed are the skill names offered when Restrict is set; unknown names
	// are ignored.
	Allowed []string `json:"allowed,omitempty"`
	// Pinned are skills injected for this run in addition to Config.Pinned,
	// whether or not they are offered, with their tools unlocked as for
	// Config.Pinned; unknown names are ignored.
	Pinned []string `json:"pinned,omitempty"`
}

// Keys of the run state. All are private: they are not inherited blindly by
// subagents, and the loaded list is bulky per-run data. ScopeKey is exported
// because hosts write it; read the loaded skills with Loaded.
var (
	// ScopeKey holds the run's Scope; absent means the zero Scope.
	ScopeKey = state.NewKey[Scope]("skills.scope", state.Private[Scope]())
	// loadedKey holds the names of the skills loaded in the run, in load order.
	loadedKey = state.NewKey[[]string]("skills.loaded", state.Private[[]string]())
	// pinnedUnlocksKey records the unlock patterns BeforeAgent granted for
	// pinned skills, so a later run can take back those of pins that are gone.
	pinnedUnlocksKey = state.NewKey[[]string]("skills.pinned_unlocks", state.Private[[]string]())
)

// Loaded returns the names of the skills loaded in the run, in load order.
func Loaded(s *state.State) []string {
	names, _ := loadedKey.Get(s)
	return slices.Clone(names)
}

// Config configures the skills middleware.
type Config struct {
	// Sources are read in order; a later source replaces an earlier skill of
	// the same name.
	Sources []Source
	// Skills are programmatic skills, applied after Sources, so they win. A
	// later entry replaces an earlier one of the same name. Each must pass
	// Validate with an empty directory name.
	Skills []Skill
	// Pinned names skills whose body is injected for every run and whose
	// allowed-tools and MCP servers are unlocked from its start. Each must
	// exist.
	Pinned []string
	// Tools are skill-scoped tools, offered gated (deferred.Gate): they are
	// refused until a loaded skill's allowed-tools names them.
	Tools []tool.Tool
	// Prompt is the guidance placed before the catalog. nil uses
	// DefaultPrompt; a pointer to "" lists the catalog without guidance.
	Prompt *string
}

// Middleware offers the loaded skills to the model. It is immutable after New,
// so concurrent runs may share it.
type Middleware struct {
	skills   []Skill // sorted by name
	byName   map[string]Skill
	warnings []error
	pinned   []string
	tools    []tool.Tool // the skill tool, then each Config.Tools entry gated
	prompt   string
}

var (
	_ agent.Middleware   = (*Middleware)(nil)
	_ agent.ToolProvider = (*Middleware)(nil)
	_ agent.ModelWrapper = (*Middleware)(nil)
	_ agent.BeforeAgent  = (*Middleware)(nil)
	_ Definer            = (*Middleware)(nil)
)

// New loads the sources and returns the middleware. It fails when a source is
// unreadable, a programmatic skill is invalid, or a pinned name is unknown;
// invalid skills found in the sources are skipped and reported by Warnings.
func New(cfg Config) (*Middleware, error) {
	loaded, warnings, err := Load(cfg.Sources...)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Skill, len(loaded)+len(cfg.Skills))
	for _, s := range loaded {
		byName[s.Name] = s
	}
	for _, s := range cfg.Skills {
		if err := validate(s, ""); err != nil {
			return nil, fmt.Errorf("skills: programmatic skill %q: %w", s.Name, err)
		}
		s.Body, _ = promptsafe.CapBytes(s.Body, MaxBodyBytes)
		byName[s.Name] = s.clone() // the caller keeps its own slices
	}
	for _, name := range cfg.Pinned {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("skills: pinned skill %q is not defined", name)
		}
	}
	m := &Middleware{
		byName:   byName,
		warnings: warnings,
		pinned:   slices.Clone(cfg.Pinned),
		prompt:   DefaultPrompt,
	}
	m.tools = []tool.Tool{m.skillTool()}
	for _, t := range cfg.Tools {
		m.tools = append(m.tools, deferred.Gate(t))
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	for _, s := range byName {
		m.skills = append(m.skills, s)
	}
	slices.SortFunc(m.skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return m, nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Tools implements agent.ToolProvider: the skill tool, then every
// Config.Tools entry wrapped by deferred.Gate.
func (m *Middleware) Tools() []tool.Tool { return slices.Clone(m.tools) }

// Has implements Definer: it reports whether the middleware defines a skill
// named name.
func (m *Middleware) Has(name string) bool {
	_, ok := m.byName[name]
	return ok
}

// Skills returns the skills the middleware knows, sorted by name. Each is a
// deep copy: changing its slices or Metadata does not affect the middleware.
func (m *Middleware) Skills() []Skill {
	out := make([]Skill, len(m.skills))
	for i, s := range m.skills {
		out[i] = s.clone()
	}
	return out
}

// Warnings returns one error per skill that was skipped while loading the
// sources.
func (m *Middleware) Warnings() []error { return slices.Clone(m.warnings) }
