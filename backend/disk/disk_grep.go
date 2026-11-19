package disk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
)

// Grep searches file contents under opts.Path. When ripgrep is available (and
// enabled) it only pre-selects candidate files; every candidate is then
// searched by the same Go code as the built-in walker (RE2, binary detection,
// glob, size cap), so both paths return identical results. The one remaining
// gap: a file whose lines ripgrep's regex engine would not match but RE2 would
// is missed on the ripgrep path. Symlinks are never followed, files over
// backend.MaxGrepFileBytes are skipped, and .git and node_modules are skipped.
func (b *Backend) Grep(ctx context.Context, pattern string, opts backend.GrepOptions) ([]backend.GrepMatch, error) {
	clean, rel, info, err := b.resolveBase("grep", opts.Path)
	if err != nil {
		return nil, err
	}
	grepper, err := backend.NewGrepper(pattern, opts, clean)
	if err != nil {
		return nil, err
	}
	s := search{backend: b, grepper: grepper, clean: clean, rel: rel}
	if !info.IsDir() {
		return grepFile(s), nil
	}
	if b.rgPath != "" {
		if candidates, ok := b.ripgrepCandidates(ctx, pattern, opts, filepath.Join(b.root, rel)); ok {
			return grepCandidates(s, candidates), nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return grepWalk(ctx, s)
}

// search describes one Grep call after its base path has been resolved.
type search struct {
	backend *Backend
	grepper *backend.Grepper
	clean   string // virtual path searched
	rel     string // resolved path searched, relative to the root
}

// readForGrep reads the file at rel (relative to the root) unless it is not a
// regular file or is too large to be worth searching.
func (s search) readForGrep(rel string) ([]byte, bool) {
	content, _, err := s.backend.readRegular(rel, backend.MaxGrepFileBytes)
	return content, err == nil
}

// grepFile searches the single file a Grep call was pointed at.
func grepFile(s search) []backend.GrepMatch {
	if s.grepper.WantsPath(s.clean) {
		if content, ok := s.readForGrep(s.rel); ok {
			s.grepper.AddFile(s.clean, content)
		}
	}
	return s.grepper.Results()
}

func grepWalk(ctx context.Context, s search) ([]backend.GrepMatch, error) {
	err := s.backend.walkFiles(ctx, s.rel, skippedDirs, func(rel string, _ fs.DirEntry) error {
		if s.addRelative(rel) {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("disk: grep %s: %w", s.clean, err)
	}
	return s.grepper.Results(), nil
}

// grepCandidates searches ripgrep's candidate files (slash-separated paths
// relative to the search directory) in walk order, stopping once the result
// cap is reached.
func grepCandidates(s search, candidates []string) []backend.GrepMatch {
	slices.SortFunc(candidates, backend.ComparePaths)
	for _, rel := range candidates {
		if s.addRelative(rel) {
			break
		}
	}
	return s.grepper.Results()
}

// addRelative searches the file at rel below the search directory and reports
// whether the result cap has been reached.
func (s search) addRelative(rel string) (full bool) {
	virtualPath := path.Join(s.clean, rel)
	if !s.grepper.WantsPath(virtualPath) {
		return false
	}
	content, ok := s.readForGrep(filepath.Join(s.rel, filepath.FromSlash(rel)))
	return ok && s.grepper.AddFile(virtualPath, content)
}

// ripgrepCandidates asks ripgrep which files under the real directory dir may
// contain a match. ok is false when ripgrep failed (for example its regex
// engine rejected the pattern) and the caller should walk the tree itself.
// Paths are streamed NUL-separated, so memory is bounded by the file count,
// not by match volume. Ripgrep only names candidates; reading them goes
// through the root like every other file operation.
func (b *Backend) ripgrepCandidates(ctx context.Context, pattern string, opts backend.GrepOptions, dir string) (candidates []string, ok bool) {
	cmd := exec.CommandContext(ctx, b.rgPath, rgArgs(pattern, opts)...)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return nil, false
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Split(splitNUL)
	for scanner.Scan() {
		candidates = append(candidates, filepath.ToSlash(strings.TrimPrefix(scanner.Text(), "./")))
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	if scanErr != nil || (waitErr != nil && !(errors.As(waitErr, &exitErr) && exitErr.ExitCode() == rgExitNoMatch)) {
		return nil, false
	}
	return candidates, true
}

// rgExitNoMatch is ripgrep's exit status when the search succeeded but found
// nothing.
const rgExitNoMatch = 1

// rgArgs builds the ripgrep command line. It lists candidate files only; glob
// filtering, binary detection and the real matching are left to Go. Ignore
// files and config are disabled so every file the walker would visit is a
// candidate; the pattern travels via -e so it can never be read as a flag, and
// no shell is involved.
func rgArgs(pattern string, opts backend.GrepOptions) []string {
	args := []string{
		"--files-with-matches", "--null", "--no-config", "--no-ignore", "--hidden", "--crlf",
		"--glob", "!.git", "--glob", "!node_modules",
	}
	if opts.Literal {
		args = append(args, "-F")
	}
	if opts.IgnoreCase {
		args = append(args, "-i")
	}
	return append(args, "-e", pattern, "--", ".")
}

// splitNUL is a bufio.SplitFunc for NUL-terminated records.
func splitNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
