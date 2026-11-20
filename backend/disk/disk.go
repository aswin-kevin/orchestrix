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
// Races: every path is checked up front, which yields ErrOutsideRoot, and
// every file operation then goes through an os.Root opened on the root
// directory, which refuses to leave it at the moment of the call. So a
// concurrent writer, such as a background shell job, that swaps a directory
// for a symlink after the check can make an operation fail but never redirect
// it outside the root.
package disk

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
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
	// tempPrefix starts the names of the temporary files used for atomic
	// writes.
	tempPrefix = ".orchestrix-write-"
)

// skippedDirs are never descended into by Grep.
var skippedDirs = map[string]bool{".git": true, "node_modules": true}

// Backend is a backend.Backend rooted at a directory. The directory stays open
// until Close; on Windows it cannot be deleted or renamed while it is open.
type Backend struct {
	root    string   // symlink-free path of the root directory
	fsRoot  *os.Root // every file operation goes through it
	virtual bool
	ripgrep bool
	rgPath  string // empty when ripgrep is disabled or not installed
}

var _ backend.Backend = (*Backend)(nil)

// Option configures a Backend.
type Option func(*Backend)

// WithVirtual chooses how paths map to the file system. When true (the
// default) "/a/b" means <root>/a/b. When false, paths are real file-system
// paths, which must still lie inside the root. On Windows such a path starts
// with its drive ("C:\work\a.txt" or "C:/work/a.txt"), and the paths the
// backend returns use forward slashes ("C:/work/a.txt"); a root on a UNC
// share (\\server\share) needs the default, virtual paths.
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
	fsRoot, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("disk: root %q: %w", root, err)
	}
	b := &Backend{root: resolved, fsRoot: fsRoot, virtual: true, ripgrep: true}
	for _, opt := range opts {
		opt(b)
	}
	if b.ripgrep {
		b.rgPath, _ = exec.LookPath("rg")
	}
	return b, nil
}

// Close releases the root directory. Operations after Close fail. Close is
// idempotent. Agent.Close calls it for a backend given with WithBackend or
// WithShell.
func (b *Backend) Close() error {
	return b.fsRoot.Close()
}

// rootPath is the path that denotes the root directory.
func (b *Backend) rootPath() string {
	if b.virtual {
		return virtualRoot
	}
	return filepath.ToSlash(b.root)
}

// clean cleans p as backend.Clean does, except for a non-virtual backend's
// host path with a volume name (on Windows, "C:\work\a.txt"), which keeps
// its volume and comes back slash-separated ("C:/work/a.txt"), the form of
// every path the backend returns. backend.Clean would turn it into
// "/C:\work\a.txt", which names nothing.
func (b *Backend) clean(p string) (string, error) {
	if b.virtual || filepath.VolumeName(p) == "" {
		return backend.Clean(p)
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: contains NUL byte", backend.ErrInvalidPath)
	}
	return filepath.ToSlash(filepath.Clean(p)), nil
}

// resolve cleans p and returns it together with the symlink-free real path it
// denotes, failing with ErrOutsideRoot when that path leaves the root. The
// real path is what callers must operate on, so a symlink inside the root
// cannot redirect an operation after the check.
func (b *Backend) resolve(op, p string) (clean, realPath string, err error) {
	clean, err = b.clean(p)
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

// resolveRel is resolve for file operations: it returns the resolved path
// relative to the root, the form b.fsRoot takes.
func (b *Backend) resolveRel(op, p string) (clean, rel string, err error) {
	clean, realPath, err := b.resolve(op, p)
	if err != nil {
		return "", "", err
	}
	rel, err = filepath.Rel(b.root, realPath)
	if err != nil {
		return "", "", wrapFSError(op, clean, err)
	}
	return clean, rel, nil
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
func (b *Backend) resolveBase(op, p string) (clean, rel string, info fs.FileInfo, err error) {
	if p == "" {
		p = b.rootPath()
	}
	clean, rel, err = b.resolveRel(op, p)
	if err != nil {
		return "", "", nil, err
	}
	info, err = b.fsRoot.Stat(rel)
	if err != nil {
		return "", "", nil, wrapFSError(op, clean, err)
	}
	return clean, rel, info, nil
}

// resolveDir is resolveBase for operations that need a directory.
func (b *Backend) resolveDir(op, p string) (clean, rel string, err error) {
	clean, rel, info, err := b.resolveBase(op, p)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("disk: %s %s: %w", op, clean, backend.ErrNotDir)
	}
	return clean, rel, nil
}

// Ls lists the direct children of the directory at p. Symlinks that resolve
// outside the root are omitted.
func (b *Backend) Ls(_ context.Context, p string) ([]backend.FileInfo, error) {
	clean, rel, err := b.resolveDir("ls", p)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(b.fsRoot.FS(), filepath.ToSlash(rel))
	if err != nil {
		return nil, wrapFSError("ls", clean, err)
	}
	out := make([]backend.FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, ok := b.entryInfo(rel, entry)
		if !ok {
			continue
		}
		out = append(out, toFileInfo(path.Join(clean, entry.Name()), info))
	}
	return out, nil
}

// entryInfo returns the info for an entry of the directory dir (relative to
// the root), following symlinks that stay inside the root. ok is false for
// entries that must not be exposed.
func (b *Backend) entryInfo(dir string, entry fs.DirEntry) (info fs.FileInfo, ok bool) {
	if entry.Type()&fs.ModeSymlink == 0 {
		info, err := entry.Info()
		return info, err == nil
	}
	// The root follows a symlink only while it stays inside the root.
	info, err := b.fsRoot.Stat(filepath.Join(dir, entry.Name()))
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
// and root-relative paths for operations that write the file back.
func (b *Backend) load(op, p string) (clean, rel string, content []byte, info fs.FileInfo, err error) {
	clean, rel, err = b.resolveRel(op, p)
	if err != nil {
		return "", "", nil, nil, err
	}
	content, info, err = b.readRegular(rel, backend.MaxReadFileBytes)
	if err != nil {
		return "", "", nil, nil, wrapFSError(op, clean, err)
	}
	return clean, rel, content, info, nil
}

// readRegular reads the regular file at rel (relative to the root), refusing
// directories (ErrIsDir), other non-regular files and files over limit bytes
// (a *backend.TooLargeError) before reading them. The file is checked before it is opened, because
// opening a FIFO can block forever, and checked again once open, in case it
// was replaced in between.
func (b *Backend) readRegular(rel string, limit int64) ([]byte, fs.FileInfo, error) {
	info, err := b.fsRoot.Stat(rel)
	if err != nil {
		return nil, nil, err
	}
	if err := checkReadable(info, limit); err != nil {
		return nil, nil, err
	}
	f, err := b.fsRoot.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if err := checkReadable(info, limit); err != nil {
		return nil, nil, err
	}
	// The limit also bounds a file that grows while it is read.
	content, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if size := int64(len(content)); size > limit {
		return nil, nil, &backend.TooLargeError{Size: size}
	}
	return content, info, nil
}

// checkReadable rejects what readRegular refuses to read.
func checkReadable(info fs.FileInfo, limit int64) error {
	switch {
	case info.IsDir():
		return backend.ErrIsDir
	case !info.Mode().IsRegular():
		return errNotRegular
	case info.Size() > limit:
		return &backend.TooLargeError{Size: info.Size()}
	}
	return nil
}

// errNotRegular reports a FIFO, device or socket, which the backend refuses to
// open because reading one can block forever.
var errNotRegular = errors.New("not a regular file")

// Write replaces the file at p atomically, creating parent directories.
func (b *Backend) Write(_ context.Context, p string, content []byte) error {
	clean, rel, err := b.resolveRel("write", p)
	if err != nil {
		return err
	}
	if err := b.replaceFile(rel, content); err != nil {
		return wrapFSError("write", clean, err)
	}
	return nil
}

// Edit replaces old with replacement in the file at p.
func (b *Backend) Edit(_ context.Context, p, old, replacement string, replaceAll bool) (backend.EditResult, error) {
	clean, rel, content, _, err := b.load("edit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	updated, count, err := backend.ApplyEdit(content, old, replacement, replaceAll)
	if err != nil {
		return backend.EditResult{}, fmt.Errorf("disk: edit %s: %w", clean, err)
	}
	if err := b.replaceFile(rel, updated); err != nil {
		return backend.EditResult{}, wrapFSError("edit", clean, err)
	}
	return backend.EditResult{Occurrences: count}, nil
}

// MultiEdit applies edits to the file at p all or nothing: the edits run on
// the file's bytes in memory and a single atomic replace writes the result.
func (b *Backend) MultiEdit(_ context.Context, p string, edits []backend.EditOp) (backend.EditResult, error) {
	clean, rel, content, _, err := b.load("multi-edit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	updated, count, err := backend.ApplyEdits(content, edits)
	if err != nil {
		return backend.EditResult{}, fmt.Errorf("disk: multi-edit %s: %w", clean, err)
	}
	if err := b.replaceFile(rel, updated); err != nil {
		return backend.EditResult{}, wrapFSError("multi-edit", clean, err)
	}
	return backend.EditResult{Occurrences: count}, nil
}

// tempCreated, when set, runs on each temporary file replaceFile creates,
// right after creating it. Tests use it to inspect the file mid-rewrite.
var tempCreated func(*os.File)

// replaceFile atomically replaces the file at rel (relative to the root) with
// content: it writes a temporary file next to it and renames that over the
// target, so readers never see a partial file. An existing file keeps its mode.
// The temporary file is created with that mode, so the new content is never
// readable more widely than the file it replaces; the umask may narrow it at
// creation, and the Chmod before the rename restores the exact bits.
func (b *Backend) replaceFile(rel string, content []byte) error {
	mode := newFileMode
	info, err := b.fsRoot.Stat(rel)
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
	dir := filepath.Dir(rel)
	if err := b.fsRoot.MkdirAll(dir, newDirMode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// A parent exists but is not a directory. Unix reports ENOTDIR,
			// Windows "file exists"; report ENOTDIR on both.
			return &fs.PathError{Op: "mkdir", Path: dir, Err: syscall.ENOTDIR}
		}
		return err
	}
	tmpName := filepath.Join(dir, tempPrefix+rand.Text())
	tmp, err := b.fsRoot.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if tempCreated != nil {
		tempCreated(tmp)
	}
	committed := false
	defer func() {
		if !committed {
			_ = b.fsRoot.Remove(tmpName)
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
	if err := b.fsRoot.Rename(tmpName, rel); err != nil {
		return err
	}
	committed = true
	return nil
}

// Delete removes the file or directory tree at p. A symlink is removed
// itself, never its target. The root cannot be deleted.
func (b *Backend) Delete(_ context.Context, p string) error {
	clean, err := b.clean(p)
	if err != nil {
		return err
	}
	// Resolve only the parent so that a final symlink is deleted, not followed.
	_, parent, err := b.resolveRel("delete", path.Dir(clean))
	if err != nil {
		return err
	}
	target := filepath.Join(parent, path.Base(clean))
	if target == "." {
		return fmt.Errorf("disk: delete %s: %w: cannot delete the root", clean, backend.ErrInvalidPath)
	}
	if _, err := b.fsRoot.Lstat(target); err != nil {
		return wrapFSError("delete", clean, err)
	}
	if err := b.fsRoot.RemoveAll(target); err != nil {
		return wrapFSError("delete", clean, err)
	}
	return nil
}

// Glob returns the regular files under base matching pattern, newest first. A
// relative pattern is resolved against base; an empty base means the root.
// Symlinks are not followed or listed.
func (b *Backend) Glob(ctx context.Context, pattern, base string) ([]backend.FileInfo, error) {
	clean, rel, err := b.resolveDir("glob", base)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = path.Join(clean, pattern)
	}
	var out []backend.FileInfo
	err = b.walkFiles(ctx, rel, nil, func(fileRel string, d fs.DirEntry) error {
		virtualPath := path.Join(clean, fileRel)
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

// walkFiles calls fn for every regular file below dir (relative to the root),
// in directory-walk order, passing the slash-separated path relative to dir.
// Symlinks and directories named in skip are not entered; unreadable entries
// are ignored. fn may return filepath.SkipAll to stop early.
func (b *Backend) walkFiles(ctx context.Context, dir string, skip map[string]bool, fn func(rel string, d fs.DirEntry) error) error {
	start := filepath.ToSlash(dir)
	return fs.WalkDir(b.fsRoot.FS(), start, func(p string, d fs.DirEntry, err error) error {
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
			if p != start && skip[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return fn(relativeTo(start, p), d)
	})
}

// relativeTo returns the slash-separated path p, which lies below dir, relative
// to dir; both are paths as an fs.FS names them, where "." is the root.
func relativeTo(dir, p string) string {
	if dir == "." {
		return p
	}
	return strings.TrimPrefix(p, dir+"/")
}
