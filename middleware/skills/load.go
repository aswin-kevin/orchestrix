package skills

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/internal/frontmatter"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
)

// Source is a place holding skills, one directory per skill.
type Source struct {
	// FS is the file system to read. Required.
	FS fs.FS
	// Dir is the root inside FS that holds the skill directories; "" means
	// the root of FS.
	Dir string
}

// DirSource returns a Source for the directory at path on disk.
func DirSource(path string) Source { return Source{FS: os.DirFS(path), Dir: "."} }

// frontmatterFields are the SKILL.md keys Orchestrix reads; others are ignored.
type frontmatterFields struct {
	Name          string                 `yaml:"name"`
	Description   string                 `yaml:"description"`
	License       string                 `yaml:"license"`
	Compatibility string                 `yaml:"compatibility"`
	Metadata      frontmatter.Metadata   `yaml:"metadata"`
	AllowedTools  frontmatter.StringList `yaml:"allowed-tools"`
	MCPServers    frontmatter.StringList `yaml:"mcp-servers"`
}

// maxSkillFileBytes bounds what is read of one SKILL.md: the largest
// frontmatter block plus the largest body, with room for the fences.
const maxSkillFileBytes = frontmatter.MaxBytes + MaxBodyBytes + 64

// Load reads every <Dir>/<name>/SKILL.md of every source. A later source
// replaces an earlier skill of the same name. Invalid skills are skipped and
// reported in warnings; a directory without a SKILL.md is not a skill and is
// ignored; err is only for an unreadable source. The result is sorted by name.
func Load(sources ...Source) (skills []Skill, warnings []error, err error) {
	byName := map[string]Skill{}
	for _, src := range sources {
		found, warn, err := loadSource(src)
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, warn...)
		for _, s := range found {
			byName[s.Name] = s
		}
	}
	for _, s := range byName {
		skills = append(skills, s)
	}
	slices.SortFunc(skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return skills, warnings, nil
}

func loadSource(src Source) ([]Skill, []error, error) {
	root := src.Dir
	if root == "" {
		root = "."
	}
	if src.FS == nil {
		return nil, nil, errors.New("skills: source has no FS")
	}
	entries, err := fs.ReadDir(src.FS, root)
	if err != nil {
		return nil, nil, fmt.Errorf("skills: read source %q: %w", root, err)
	}
	var skills []Skill
	var warnings []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := path.Join(root, e.Name())
		s, ok, err := loadSkill(src.FS, dir, e.Name())
		if err != nil {
			warnings = append(warnings, fmt.Errorf("skills: %s: %w", path.Join(dir, SkillFile), err))
		} else if ok {
			skills = append(skills, s)
		}
	}
	return skills, warnings, nil
}

// loadSkill reads one skill directory. ok is false when it has no regular
// SKILL.md; a SKILL.md that is a symlink does not count, so a skill directory
// cannot pull in a file from elsewhere.
func loadSkill(fsys fs.FS, dir, dirName string) (s Skill, ok bool, err error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Skill{}, false, err
	}
	if !slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return e.Name() == SkillFile && e.Type().IsRegular() }) {
		return Skill{}, false, nil
	}
	data, err := readCapped(fsys, path.Join(dir, SkillFile))
	if err != nil {
		return Skill{}, false, err
	}
	var fm frontmatterFields
	body, found, err := frontmatter.Parse(data, &fm)
	if err != nil {
		return Skill{}, false, err
	}
	if !found {
		return Skill{}, false, errors.New("missing frontmatter")
	}
	body, _ = promptsafe.CapBytes(body, MaxBodyBytes)
	s = Skill{
		Name:          fm.Name,
		Description:   fm.Description,
		License:       fm.License,
		Compatibility: fm.Compatibility,
		Metadata:      fm.Metadata,
		AllowedTools:  fm.AllowedTools,
		MCPServers:    fm.MCPServers,
		Body:          body,
		FS:            fsys,
		Dir:           dir,
		Files:         listFiles(fsys, dir),
	}
	if err := Validate(s, dirName); err != nil {
		return Skill{}, false, err
	}
	return s, true, nil
}

func readCapped(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxSkillFileBytes))
}

// listFiles returns the regular files under dir other than the top-level
// SKILL.md, as sorted slash paths relative to dir. Symlinks are neither listed
// nor followed.
func listFiles(fsys fs.FS, dir string) []string {
	var files []string
	_ = fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel := strings.TrimPrefix(p, dir+"/")
		if rel == SkillFile {
			return nil
		}
		if files = append(files, rel); len(files) >= MaxListedFiles {
			return fs.SkipAll
		}
		return nil
	})
	slices.Sort(files)
	return files
}
