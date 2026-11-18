// Package disk provides a backend.Backend over a directory on the real file
// system. It is a security boundary: every path is resolved through symlinks
// and must stay inside the root, otherwise the call fails with
// backend.ErrOutsideRoot (or, in directory walks, the entry is skipped).
//
// Threat model: hard links are not detected, so a file inside the root that
// was already hard-linked to a file outside it can be read (writes are safe,
// because they replace the file by rename instead of writing through the
// link). Do not point the backend at a directory an untrusted party can
// pre-populate with hard links.
//
// Race note: the containment check and the file operation are separate system
// calls, so a process that can rewrite symlinks inside the root concurrently
// could still win a race. The backend defends against the agent, not against
// other writers of the same directory.
package disk

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aswin-kevin/orchestrix/backend"
)

const (
	virtualRoot = "/"
	// newFileMode and newDirMode apply to files and directories the backend
	// creates; existing files keep their mode when rewritten.
	newFileMode os.FileMode = 0o644
	newDirMode  os.FileMode = 0o755
	// tempPattern names the temporary files used for atomic writes.
	tempPattern = ".orchestrix-write-*"
)

// skippedDirs are never descended into by Grep.
var skippedDirs = map[string]bool{".git": true, "node_modules": true}

// Backend is a backend.Backend rooted at a directory.
type Backend struct {
	root    string
	virtual bool
	ripgrep bool
	rgPath  string // empty when ripgrep is disabled or not installed
}

var _ backend.Backend = (*Backend)(nil)

// Option configures a Backend.
type Option func(*Backend)

// WithVirtual chooses how paths map to the file system. When true (the
// default) "/a/b" means <root>/a/b. When false, paths are real file-system
// paths, which must still lie inside the root.
func WithVirtual(virtual bool) Option {
	return func(b *Backend) { b.virtual = virtual }
}

// WithRipgrep enables (the default) or disables use of the rg binary for Grep.
// Even when enabled, rg is used only if it is found on PATH.
func WithRipgrep(enabled bool) Option {
	return func(b *Backend) { b.ripgrep = enabled }
}

// New returns a Backend rooted at root, which must be an existing directory.
func New(root string, opts ...Option) (*Backend, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("disk: root %q: %w", root, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("disk: root %q: %w", root, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("disk: root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("disk: root %q: %w", root, backend.ErrNotDir)
	}
	b := &Backend{root: resolved, virtual: true, ripgrep: true}
	for _, opt := range opts {
		opt(b)
	}
	if b.ripgrep {
		b.rgPath, _ = exec.LookPath("rg")
	}
	return b, nil
}

// rootPath is the virtual path that denotes the root directory.
func (b *Backend) rootPath() string {
	if b.virtual {
		return virtualRoot
	}
	return b.root
}

// resolve cleans p and returns it together with the symlink-free real path it
// denotes, failing with ErrOutsideRoot when that path leaves the root. The
// real path is what callers must operate on, so a symlink inside the root
// cannot redirect an operation after the check.
func (b *Backend) resolve(op, p string) (clean, realPath string, err error) {
	clean, err = backend.Clean(p)
	if err != nil {
		return "", "", err
	}
	realPath = filepath.FromSlash(clean)
	if b.virtual {
		realPath = filepath.Join(b.root, realPath)
	}
	resolved, err := evalPath(realPath)
	if err != nil {
		return "", "", wrapFSError(op, clean, err)
	}
	if !b.inside(resolved) {
		return "", "", fmt.Errorf("disk: %s %s: %w", op, clean, backend.ErrOutsideRoot)
	}
	return clean, resolved, nil
}

// RealPath returns the symlink-free host path that p denotes, after the same
// cleaning, symlink resolution and confinement checks as Read; it fails with
// backend.ErrOutsideRoot when the path leaves the root. An empty p means the
// root. The path need not exist.
func (b *Backend) RealPath(p string) (string, error) {
	if p == "" {
		p = b.rootPath()
	}
	_, realPath, err := b.resolve("realpath", p)
	return realPath, err
}

func (b *Backend) inside(p string) bool {
	rel, err := filepath.Rel(b.root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// evalPath resolves symlinks in p. For a path that does not exist yet it
// resolves the nearest existing ancestor and appends the missing tail, so new
// files are checked against where they would really land. A dangling symlink
// is an error: writing through it would create a file at its (unchecked)
// target.
func evalPath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if _, statErr := os.Lstat(p); statErr == nil {
		return "", fmt.Errorf("dangling symlink %s: %w", p, fs.ErrNotExist)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return "", err
	}
	resolvedParent, err := evalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(p)), nil
}

// wrapFSError attaches context to err and maps OS errors onto the backend
// sentinels.
func wrapFSError(op, p string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("disk: %s %s: %w", op, p, backend.ErrNotFound)
	case errors.Is(err, syscall.ENOTDIR):
		return fmt.Errorf("disk: %s %s: %w", op, p, backend.ErrNotDir)
	}
	return fmt.Errorf("disk: %s %s: %w", op, p, err)
}

// resolveBase resolves p (empty means the root) and stats the result.
func (b *Backend) resolveBase(op, p string) (clean, realPath string, info fs.FileInfo, err error) {
	if p == "" {
		p = b.rootPath()
	}
	clean, realPath, err = b.resolve(op, p)
	if err != nil {
		return "", "", nil, err
	}
	info, err = os.Stat(realPath)
	if err != nil {
		return "", "", nil, wrapFSError(op, clean, err)
	}
	return clean, realPath, info, nil
}

// resolveDir is resolveBase for operations that need a directory.
func (b *Backend) resolveDir(op, p string) (clean, realPath string, err error) {
	clean, realPath, info, err := b.resolveBase(op, p)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("disk: %s %s: %w", op, clean, backend.ErrNotDir)
	}
	return clean, realPath, nil
}

// Ls lists the direct children of the directory at p. Symlinks that resolve
// outside the root are omitted.
func (b *Backend) Ls(_ context.Context, p string) ([]backend.FileInfo, error) {
	clean, realPath, err := b.resolveDir("ls", p)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(realPath)
	if err != nil {
		return nil, wrapFSError("ls", clean, err)
	}
	out := make([]backend.FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, ok := b.entryInfo(realPath, entry)
		if !ok {
			continue
		}
		out = append(out, toFileInfo(path.Join(clean, entry.Name()), info))
	}
	return out, nil
}

// entryInfo returns the info for a directory entry, following symlinks that
// stay inside the root. ok is false for entries that must not be exposed.
func (b *Backend) entryInfo(dir string, entry fs.DirEntry) (info fs.FileInfo, ok bool) {
	if entry.Type()&fs.ModeSymlink == 0 {
		info, err := entry.Info()
		return info, err == nil
	}
	target, err := evalPath(filepath.Join(dir, entry.Name()))
	if err != nil || !b.inside(target) {
		return nil, false
	}
	info, err = os.Stat(target)
	return info, err == nil
}

func toFileInfo(virtualPath string, info fs.FileInfo) backend.FileInfo {
	return backend.FileInfo{Path: virtualPath, IsDir: info.IsDir(), Size: info.Size(), ModTime: info.ModTime()}
}

// Read returns a page of the file at p.
func (b *Backend) Read(_ context.Context, p string, offset, limit int) (backend.ReadResult, error) {
	_, _, content, info, err := b.load("read", p)
	if err != nil {
		return backend.ReadResult{}, err
	}
	return backend.NewReadResult(content, info.ModTime(), offset, limit), nil
}

// load resolves p and reads the whole regular file it names, refusing one
// over backend.MaxReadFileBytes before reading it. It returns the cleaned
// and real paths for operations that write the file back.
func (b *Backend) load(op, p string) (clean, realPath string, content []byte, info fs.FileInfo, err error) {
	clean, realPath, err = b.resolve(op, p)
	if err != nil {
		return "", "", nil, nil, err
	}
	info, err = os.Stat(realPath)
	if err != nil {
		return "", "", nil, nil, wrapFSError(op, clean, err)
	}
	if err := requireRegular(op, clean, info); err != nil {
		return "", "", nil, nil, err
	}
	if err := backend.CheckReadSize(info.Size()); err != nil {
		return "", "", nil, nil, fmt.Errorf("disk: %s %s: %w", op, clean, err)
	}
	content, err = os.ReadFile(realPath)
	if err != nil {
		return "", "", nil, nil, wrapFSError(op, clean, err)
	}
	return clean, realPath, content, info, nil
}

// errNotRegular reports a FIFO, device or socket, which the backend refuses to
// open because reading one can block forever.
var errNotRegular = errors.New("not a regular file")

// requireRegular rejects directories (ErrIsDir) and other non-regular files.
func requireRegular(op, clean string, info fs.FileInfo) error {
	switch {
	case info.IsDir():
		return fmt.Errorf("disk: %s %s: %w", op, clean, backend.ErrIsDir)
	case !info.Mode().IsRegular():
		return fmt.Errorf("disk: %s %s: %w", op, clean, errNotRegular)
	}
	return nil
}

// Write replaces the file at p atomically, creating parent directories.
func (b *Backend) Write(_ context.Context, p string, content []byte) error {
	clean, realPath, err := b.resolve("write", p)
	if err != nil {
		return err
	}
	if err := replaceFile(realPath, content); err != nil {
		return wrapFSError("write", clean, err)
	}
	return nil
}

// Edit replaces old with replacement in the file at p.
func (b *Backend) Edit(_ context.Context, p, old, replacement string, replaceAll bool) (backend.EditResult, error) {
	clean, realPath, content, _, err := b.load("edit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	updated, count, err := backend.ApplyEdit(content, old, replacement, replaceAll)
	if err != nil {
		return backend.EditResult{}, fmt.Errorf("disk: edit %s: %w", clean, err)
	}
	if err := replaceFile(realPath, updated); err != nil {
		return backend.EditResult{}, wrapFSError("edit", clean, err)
	}
	return backend.EditResult{Occurrences: count}, nil
}

// MultiEdit applies edits to the file at p all or nothing: the edits run on
// the file's bytes in memory and a single atomic replace writes the result.
func (b *Backend) MultiEdit(_ context.Context, p string, edits []backend.EditOp) (backend.EditResult, error) {
	clean, realPath, content, _, err := b.load("multi-edit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	updated, count, err := backend.ApplyEdits(content, edits)
	if err != nil {
		return backend.EditResult{}, fmt.Errorf("disk: multi-edit %s: %w", clean, err)
	}
	if err := replaceFile(realPath, updated); err != nil {
		return backend.EditResult{}, wrapFSError("multi-edit", clean, err)
	}
	return backend.EditResult{Occurrences: count}, nil
}

// replaceFile atomically replaces the file at real (a resolved path) with
// content: it writes a temporary file next to it and renames that over the
// target, so readers never see a partial file. An existing file keeps its mode.
func replaceFile(realPath string, content []byte) error {
	mode := newFileMode
	info, err := os.Stat(realPath)
	switch {
	case err == nil && info.IsDir():
		return backend.ErrIsDir
	case err == nil && !info.Mode().IsRegular():
		return errNotRegular
	case err == nil:
		mode = info.Mode().Perm()
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	dir := filepath.Dir(realPath)
	if err := os.MkdirAll(dir, newDirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), realPath); err != nil {
		return err
	}
	committed = true
	return nil
}

// Delete removes the file or directory tree at p. A symlink is removed
// itself, never its target. The root cannot be deleted.
func (b *Backend) Delete(_ context.Context, p string) error {
	clean, err := backend.Clean(p)
	if err != nil {
		return err
	}
	// Resolve only the parent so that a final symlink is deleted, not followed.
	_, parent, err := b.resolve("delete", path.Dir(clean))
	if err != nil {
		return err
	}
	target := filepath.Join(parent, path.Base(clean))
	if target == b.root {
		return fmt.Errorf("disk: delete %s: %w: cannot delete the root", clean, backend.ErrInvalidPath)
	}
	if _, err := os.Lstat(target); err != nil {
		return wrapFSError("delete", clean, err)
	}
	if err := os.RemoveAll(target); err != nil {
		return wrapFSError("delete", clean, err)
	}
	return nil
}

// Glob returns the regular files under base matching pattern, newest first. A
// relative pattern is resolved against base; an empty base means the root.
// Symlinks are not followed or listed.
func (b *Backend) Glob(ctx context.Context, pattern, base string) ([]backend.FileInfo, error) {
	clean, realPath, err := b.resolveDir("glob", base)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = path.Join(clean, pattern)
	}
	var out []backend.FileInfo
	err = walkFiles(ctx, realPath, nil, func(rel string, d fs.DirEntry) error {
		virtualPath := path.Join(clean, rel)
		if !backend.MatchGlob(pattern, virtualPath) {
			return nil
		}
		if info, err := d.Info(); err == nil {
			out = append(out, toFileInfo(virtualPath, info))
		}
		return nil
	})
	if err != nil {
		return nil, wrapFSError("glob", clean, err)
	}
	backend.SortGlobResults(out)
	return out, nil
}

// walkFiles calls fn for every regular file below dir, in directory-walk
// order, passing the slash-separated path relative to dir. Symlinks and
// directories named in skip are not entered; unreadable entries are ignored.
// fn may return filepath.SkipAll to stop early.
func walkFiles(ctx context.Context, dir string, skip map[string]bool, fn func(rel string, d fs.DirEntry) error) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != dir && skip[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		return fn(filepath.ToSlash(rel), d)
	})
}
