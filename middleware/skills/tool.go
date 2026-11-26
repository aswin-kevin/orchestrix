package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/internal/limitio"
	"github.com/aswin-kevin/orchestrix/internal/names"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Input is the input of the skill tool.
type Input struct {
	// Name is the skill to load.
	Name string `json:"name" desc:"The skill to load."`
	// File is a file inside the skill directory to read instead of its
	// instructions.
	File string `json:"file,omitempty" desc:"A file inside the skill directory to read instead of its instructions."`
}

// skillEnd closes the element Marker opens.
const skillEnd = "</skill>"

const (
	toolDescription = "Load a skill by name: returns its instructions, which stay in effect for the rest of the conversation, and lists the files that come with it. With file, returns that file of the skill instead."
	filesIntro      = "\nFiles in this skill (read one with the skill tool's file parameter): "
)

// Marker is the opening tag that frames the instructions of the skill
// named name, in the skill tool's result and wherever they are injected.
func Marker(name string) string { return `<skill name="` + name + `">` }

// frame renders s's body inside its skill element. Frame tags in the body are
// neutralized, so the body cannot end the element early; trailing newlines
// are dropped, so the closing tag follows the last line.
func frame(s Skill) string {
	body, _ := promptsafe.CapBytes(neutralizeTags(s.Body), MaxBodyBytes)
	return Marker(s.Name) + "\n" + strings.TrimRight(body, "\n") + "\n" + skillEnd
}

// skillTool builds the skill tool. It only reads skill files and records
// the load in the run's State, so it is read-only and safe to run in
// parallel.
func (m *Middleware) skillTool() tool.Tool {
	return tool.New(ToolSkill, toolDescription, m.runSkill, tool.WithReadOnly(), tool.WithConcurrent())
}

// runSkill loads the skill in.Name for the run, or reads in.File of it.
// Loading records the name in loadedKey and unlocks the skill's allowed tools
// and the tools of its MCP servers.
func (m *Middleware) runSkill(_ context.Context, in Input, c *tool.Call) (tool.Result, error) {
	scope, _ := ScopeKey.Get(c.State)
	s, ok := m.offered(scope, in.Name)
	if !ok {
		return tool.Errorf("unknown skill %q; %s", in.Name, m.offeredList(scope)), nil
	}
	if in.File != "" {
		text, err := readSkillFile(s, in.File)
		if err != nil {
			return tool.Errorf("cannot read %q of skill %s: %v", in.File, s.Name, err), nil
		}
		return tool.Text(text), nil
	}
	markLoaded(c.State, s.Name)
	unlockTools(c.State, s)
	out := frame(s)
	if len(s.Files) > 0 {
		out += filesIntro + strings.Join(s.Files, ", ")
	}
	return tool.Text(out), nil
}

// unlockTools unlocks (deferred.Unlock) the tools s names in allowed-tools
// and every tool of its MCP servers.
func unlockTools(st *state.State, s Skill) {
	deferred.Unlock(st, unlockPatterns(s)...)
}

// unlockPatterns lists the tool name patterns s unlocks: its allowed-tools
// and a prefix glob for each of its MCP servers.
func unlockPatterns(s Skill) []string {
	patterns := slices.Clone(s.AllowedTools)
	for _, server := range s.MCPServers {
		patterns = append(patterns, names.MCPPrefix(server)+"*")
	}
	return patterns
}

// offered returns the skill named name when scope offers it.
func (m *Middleware) offered(scope Scope, name string) (Skill, bool) {
	s, ok := m.byName[name]
	if !ok || scope.Restrict && !slices.Contains(scope.Allowed, name) {
		return Skill{}, false
	}
	return s, true
}

// offeredList tells the model which skills it may load under scope.
func (m *Middleware) offeredList(scope Scope) string {
	var offered []string
	for _, s := range m.skills {
		if _, ok := m.offered(scope, s.Name); ok {
			offered = append(offered, s.Name)
		}
	}
	if len(offered) == 0 {
		return "no skills are available"
	}
	return "available skills: " + strings.Join(offered, ", ")
}

// markLoaded appends name to the run's loaded skills unless it is there.
func markLoaded(s *state.State, name string) {
	loadedKey.Update(s, func(loaded []string) []string {
		if slices.Contains(loaded, name) {
			return loaded
		}
		// loaded is the stored slice itself: never append to it in place.
		return append(slices.Clone(loaded), name)
	})
}

// readSkillFile returns the text of file, a slash path relative to s's
// directory. The file must be a regular text file of at most
// MaxFileBytes inside the directory, reached without following any
// symbolic link, so a skill cannot expose files from elsewhere; this matches
// Skill.Files, which neither lists nor follows symlinks.
//
// The symlink walk and the Open that follows are separate steps, so a path
// swapped for a link in between would be followed. That race needs write
// access to the skill directory, which is trusted (see Trust in the package
// documentation), so it is not guarded against.
func readSkillFile(s Skill, file string) (string, error) {
	if s.FS == nil {
		return "", errors.New("the skill has no files")
	}
	clean := path.Clean(file)
	if path.IsAbs(file) || !fs.ValidPath(clean) || clean == "." {
		return "", errors.New("file must be a relative path inside the skill directory")
	}
	entry, err := lookupNoSymlinks(s.FS, s.Dir, clean)
	if err != nil {
		return "", err
	}
	info, err := entry.Info()
	if err != nil {
		return "", err
	}
	if info.Size() > MaxFileBytes {
		return "", fmt.Errorf("the file is %d bytes, over the limit of %d", info.Size(), MaxFileBytes)
	}
	f, err := s.FS.Open(path.Join(s.Dir, clean))
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, over, err := limitio.ReadAll(f, MaxFileBytes)
	switch {
	case err != nil:
		return "", err
	case over:
		return "", fmt.Errorf("the file is over the limit of %d bytes", MaxFileBytes)
	case backend.IsBinary(data):
		return "", errors.New("the file is binary")
	}
	return string(data), nil
}

// lookupNoSymlinks walks rel, a valid slash path, from dir one element at a
// time and returns the directory entry of its last element, which must be a
// regular file. It fails when an element is missing, a symbolic link, or
// (before the last) not a directory. fs.FS has no Lstat, but directory
// entries report a link's own type, so reading each parent sees links
// without following them.
func lookupNoSymlinks(fsys fs.FS, dir, rel string) (fs.DirEntry, error) {
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return nil, err
		}
		at := slices.IndexFunc(entries, func(e fs.DirEntry) bool { return e.Name() == part })
		if at < 0 {
			return nil, fs.ErrNotExist
		}
		entry := entries[at]
		last := i == len(parts)-1
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			return nil, errors.New("the path goes through a symbolic link")
		case last && !entry.Type().IsRegular():
			return nil, errors.New("not a regular file")
		case !last && !entry.IsDir():
			return nil, fs.ErrNotExist
		case last:
			return entry, nil
		}
		dir = path.Join(dir, part)
	}
	return nil, fs.ErrNotExist // unreachable: rel has at least one element
}
