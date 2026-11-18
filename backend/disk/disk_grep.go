package disk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
)

// Grep searches file contents under opts.Path. It delegates to ripgrep when
// available (and enabled), otherwise it walks the tree itself; both produce
// the same results. Symlinks are never followed, and .git and node_modules
// directories are skipped.
func (b *Backend) Grep(ctx context.Context, pattern string, opts backend.GrepOptions) ([]backend.GrepMatch, error) {
	clean, realPath, info, err := b.resolveBase("grep", opts.Path)
	if err != nil {
		return nil, err
	}
	grepper, err := backend.NewGrepper(pattern, opts, clean)
	if err != nil {
		return nil, err
	}
	search := search{grepper: grepper, clean: clean, realPath: realPath, isFile: !info.IsDir()}
	if b.rgPath != "" && rgUnderstandsGlob(opts.Glob) {
		return b.grepRipgrep(ctx, pattern, opts, search)
	}
	return grepWalk(ctx, search)
}

// search describes one Grep call after its base path has been resolved.
type search struct {
	grepper  *backend.Grepper
	clean    string // virtual path searched
	realPath string // resolved real path searched
	isFile   bool
}

func grepWalk(ctx context.Context, s search) ([]backend.GrepMatch, error) {
	if s.isFile {
		if s.grepper.WantsPath(s.clean) {
			if content, err := os.ReadFile(s.realPath); err == nil {
				s.grepper.AddFile(s.clean, content)
			}
		}
		return s.grepper.Results(), nil
	}
	err := walkFiles(ctx, s.realPath, skippedDirs, func(rel string, _ fs.DirEntry) error {
		virtualPath := path.Join(s.clean, rel)
		if !s.grepper.WantsPath(virtualPath) {
			return nil
		}
		content, err := os.ReadFile(filepath.Join(s.realPath, filepath.FromSlash(rel)))
		if err != nil {
			return nil
		}
		if s.grepper.AddFile(virtualPath, content) {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("disk: grep %s: %w", s.clean, err)
	}
	return s.grepper.Results(), nil
}

// rgUnderstandsGlob reports whether glob means the same to ripgrep as to the
// backend. Absolute globs address virtual paths, and a leading "!" is
// negation to ripgrep only, so those searches use the built-in walker.
func rgUnderstandsGlob(glob string) bool {
	return !strings.HasPrefix(glob, "/") && !strings.HasPrefix(glob, "!")
}

// rgExitNoMatch is ripgrep's exit status when the search succeeded but found
// nothing.
const rgExitNoMatch = 1

func (b *Backend) grepRipgrep(ctx context.Context, pattern string, opts backend.GrepOptions, s search) ([]backend.GrepMatch, error) {
	dir, target, virtualDir := s.realPath, ".", s.clean
	if s.isFile {
		dir, target, virtualDir = filepath.Dir(s.realPath), filepath.Base(s.realPath), path.Dir(s.clean)
	}
	cmd := exec.CommandContext(ctx, b.rgPath, rgArgs(pattern, opts, target)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.ExitCode() == rgExitNoMatch:
	case err != nil:
		if errors.As(err, &exitErr) {
			err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(exitErr.Stderr))
		}
		return nil, fmt.Errorf("disk: grep %s: rg: %w", s.clean, err)
	}
	byFile := parseRipgrep(out, virtualDir)
	for _, name := range sortedKeys(byFile) {
		if s.grepper.AddMatches(name, byFile[name]) {
			break
		}
	}
	return s.grepper.Results(), nil
}

// rgArgs builds the ripgrep command line. Ignore files and config are
// disabled so results match the built-in walker; the pattern travels via -e so
// it can never be read as a flag, and no shell is involved.
func rgArgs(pattern string, opts backend.GrepOptions, target string) []string {
	args := []string{
		"--json", "--no-config", "--no-ignore", "--hidden", "--crlf",
		"--glob", "!.git", "--glob", "!node_modules",
	}
	if opts.Literal {
		args = append(args, "-F")
	}
	if opts.IgnoreCase {
		args = append(args, "-i")
	}
	if opts.Glob != "" {
		args = append(args, "--glob", opts.Glob)
	}
	return append(args, "-e", pattern, "--", target)
}

// rgEvent is the subset of ripgrep's JSON message format that Grep needs.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       rgText `json:"path"`
		Lines      rgText `json:"lines"`
		LineNumber int    `json:"line_number"`
	} `json:"data"`
}

// rgText is ripgrep's text-or-bytes value; Text is nil for non-UTF-8 data.
type rgText struct {
	Text *string `json:"text"`
}

// parseRipgrep maps ripgrep's JSON output onto matching lines per virtual
// path. Matches whose path or line is not valid UTF-8 are dropped, as the
// built-in walker drops such files.
func parseRipgrep(out []byte, virtualDir string) map[string][]backend.LineMatch {
	byFile := map[string][]backend.LineMatch{}
	for _, raw := range bytes.Split(out, []byte("\n")) {
		var ev rgEvent
		if len(raw) == 0 || json.Unmarshal(raw, &ev) != nil || ev.Type != "match" {
			continue
		}
		if ev.Data.Path.Text == nil || ev.Data.Lines.Text == nil {
			continue
		}
		name := path.Join(virtualDir, filepath.ToSlash(*ev.Data.Path.Text))
		text := strings.TrimSuffix(strings.TrimSuffix(*ev.Data.Lines.Text, "\n"), "\r")
		byFile[name] = append(byFile[name], backend.LineMatch{Line: ev.Data.LineNumber, Text: text})
	}
	return byFile
}

// sortedKeys returns the map's keys in directory-walk order, which is the
// order the built-in walker reports files in.
func sortedKeys(m map[string][]backend.LineMatch) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, backend.ComparePaths)
	return keys
}
