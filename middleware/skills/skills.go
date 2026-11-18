// Package skills loads SKILL.md directories and offers them to the model: a
// catalog of names and descriptions in the system prompt, plus the full body
// of pinned skills.
//
// # Trust
//
// A skill's name, description and body come from files and are written into
// the system prompt, so whoever can write a skills directory can steer the
// agent. Only load sources you trust. The framing is protected: descriptions
// are reduced to one line, and a literal frame tag inside any skill text is
// neutralized so it cannot end its element.
package skills

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
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
	// DefaultMaxBodyBytes caps a skill's body when it is loaded.
	DefaultMaxBodyBytes = 256 << 10
	// DefaultMaxFileBytes caps a skill file read through the skill tool.
	DefaultMaxFileBytes = 256 << 10
	// DefaultMaxListedFiles caps Skill.Files.
	DefaultMaxListedFiles = 50
	// ToolSkill is the name of the tool that loads a skill.
	ToolSkill = "skill"
)

// Skill is one loaded skill.
type Skill struct {
	// Name identifies the skill; it follows the rules of Validate.
	Name string
	// Description says what the skill does and when to use it. Required.
	Description string
	// License and Compatibility are optional free text.
	License       string
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
	// DefaultMaxBodyBytes.
	Body string
	// FS and Dir locate the skill directory for file reads; both are zero for
	// a programmatic skill, which has no files.
	FS  fs.FS
	Dir string
	// Files lists the skill's other regular files as slash paths relative to
	// Dir, sorted, at most DefaultMaxListedFiles; nil if none.
	Files []string
}

// Validate reports why s is not a legal skill: the name must be 1 to
// MaxNameLen characters of [a-z0-9-] with no leading, trailing or doubled
// hyphen and, unless dirName is empty, equal to dirName; the description must
// be non-empty and at most MaxDescriptionLen bytes; the compatibility note at
// most MaxCompatibilityLen bytes; and allowed-tools must not hold a bare "*".
func Validate(s Skill, dirName string) error {
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
	if slices.Contains(s.AllowedTools, "*") {
		return fmt.Errorf(`allowed-tools must not contain a bare "*"; name the tools`)
	}
	return nil
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
	// whether or not they are offered; unknown names are ignored.
	Pinned []string `json:"pinned,omitempty"`
}

// Keys of the run state. Both are private: they are not inherited blindly by
// subagents, and the loaded list is bulky per-run data.
var (
	// ScopeKey holds the run's Scope; absent means the zero Scope.
	ScopeKey = state.NewKey[Scope]("skills.scope", state.Private[Scope]())
	// LoadedKey holds the names of the skills loaded in the run, in load order.
	LoadedKey = state.NewKey[[]string]("skills.loaded", state.Private[[]string]())
)

// Loaded returns the names of the skills loaded in the run, in load order.
func Loaded(s *state.State) []string {
	names, _ := LoadedKey.Get(s)
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
	// Pinned names skills whose body is injected for every run. Each must
	// exist.
	Pinned []string
	// Tools are skill-scoped tools, hidden until a loaded skill's
	// allowed-tools names them.
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
	tools    []tool.Tool
	prompt   string
}

var _ agent.Middleware = (*Middleware)(nil)

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
		if err := Validate(s, ""); err != nil {
			return nil, fmt.Errorf("skills: programmatic skill %q: %w", s.Name, err)
		}
		s.Body, _ = promptsafe.CapBytes(s.Body, DefaultMaxBodyBytes)
		byName[s.Name] = s
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
		tools:    slices.Clone(cfg.Tools),
		prompt:   DefaultPrompt,
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
func (m *Middleware) Name() string { return "skills" }

// Skills returns the skills the middleware knows, sorted by name.
func (m *Middleware) Skills() []Skill { return slices.Clone(m.skills) }

// Warnings returns one error per skill that was skipped while loading the
// sources.
func (m *Middleware) Warnings() []error { return slices.Clone(m.warnings) }
