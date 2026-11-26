package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

const rootPath = "/"

type file struct {
	content []byte
	modTime time.Time
}

// Backend is a concurrency-safe in-memory file system. The zero value is not
// usable; create one with New.
type Backend struct {
	mu    sync.RWMutex
	files map[string]file
	now   func() time.Time
}

var (
	_ backend.Backend     = (*Backend)(nil)
	_ backend.Snapshotter = (*Backend)(nil)
)

// Config holds the optional settings of a Backend. The zero Config is valid.
type Config struct {
	// Clock stamps modification times. The default is time.Now.
	Clock func() time.Time
}

// New returns an empty backend. The error is always nil today; it is part of
// the signature so that settings can be validated without a breaking change.
func New(cfg Config) (*Backend, error) {
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Backend{files: map[string]file{}, now: now}, nil
}

// kind classifies a cleaned path. Callers must hold b.mu.
type kind int

const (
	missing kind = iota
	isFile
	isDir
)

func (b *Backend) kindOf(p string) kind {
	if _, ok := b.files[p]; ok {
		return isFile
	}
	if p == rootPath {
		return isDir
	}
	prefix := p + "/"
	for name := range b.files {
		if strings.HasPrefix(name, prefix) {
			return isDir
		}
	}
	return missing
}

// hasFileAncestor reports whether some proper ancestor of p is a file, which
// would make p impossible to create.
func (b *Backend) hasFileAncestor(p string) bool {
	for dir := path.Dir(p); dir != rootPath; dir = path.Dir(dir) {
		if _, ok := b.files[dir]; ok {
			return true
		}
	}
	return false
}

func fail(op, p string, sentinel error) error {
	return fmt.Errorf("backend/memory: %s %s: %w", op, p, sentinel)
}

// Ls lists the direct children of the directory at p, sorted by name.
func (b *Backend) Ls(_ context.Context, p string) ([]backend.FileInfo, error) {
	p, err := backend.Clean(p)
	if err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	switch b.kindOf(p) {
	case missing:
		return nil, fail("ls", p, backend.ErrNotFound)
	case isFile:
		return nil, fail("ls", p, backend.ErrNotDir)
	}
	return b.children(p), nil
}

// children returns the entries directly below directory dir.
func (b *Backend) children(dir string) []backend.FileInfo {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	entries := map[string]backend.FileInfo{}
	for name, f := range b.files {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		child, _, nested := strings.Cut(rest, "/")
		info := backend.FileInfo{Path: prefix + child, IsDir: nested}
		if nested {
			info.ModTime = f.modTime
			if prev, seen := entries[info.Path]; seen && prev.ModTime.After(f.modTime) {
				info.ModTime = prev.ModTime
			}
		} else {
			info.Size, info.ModTime = int64(len(f.content)), f.modTime
		}
		entries[info.Path] = info
	}
	out := slices.Collect(maps.Values(entries))
	slices.SortFunc(out, func(a, c backend.FileInfo) int { return strings.Compare(a.Path, c.Path) })
	return out
}

// Read returns a page of the file at p.
func (b *Backend) Read(_ context.Context, p string, offset, limit int) (backend.ReadResult, error) {
	p, err := backend.Clean(p)
	if err != nil {
		return backend.ReadResult{}, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	f, err := b.load("read", p)
	if err != nil {
		return backend.ReadResult{}, err
	}
	return backend.NewReadResult(f.content, f.modTime, offset, limit), nil
}

// load returns the file at the cleaned path p for an operation that needs
// its whole content: it fails for a directory, a missing file, or a file over
// backend.MaxReadFileBytes. Callers must hold b.mu.
func (b *Backend) load(op, p string) (file, error) {
	f, ok := b.files[p]
	if !ok {
		if b.kindOf(p) == isDir {
			return file{}, fail(op, p, backend.ErrIsDir)
		}
		return file{}, fail(op, p, backend.ErrNotFound)
	}
	if err := backend.CheckReadSize(int64(len(f.content))); err != nil {
		return file{}, fail(op, p, err)
	}
	return f, nil
}

// Write replaces the file at p, creating parent directories implicitly.
func (b *Backend) Write(_ context.Context, p string, content []byte) error {
	p, err := backend.Clean(p)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.kindOf(p) == isDir {
		return fail("write", p, backend.ErrIsDir)
	}
	if b.hasFileAncestor(p) {
		return fail("write", p, backend.ErrNotDir)
	}
	b.files[p] = file{content: slices.Clone(content), modTime: b.now()}
	return nil
}

// Edit replaces old with replacement in the file at p.
func (b *Backend) Edit(_ context.Context, p, old, replacement string, replaceAll bool) (backend.EditResult, error) {
	p, err := backend.Clean(p)
	if err != nil {
		return backend.EditResult{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := b.load("edit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	updated, count, err := backend.ApplyEdit(f.content, old, replacement, replaceAll)
	if err != nil {
		return backend.EditResult{}, fmt.Errorf("backend/memory: edit %s: %w", p, err)
	}
	b.files[p] = file{content: updated, modTime: b.now()}
	return backend.EditResult{Occurrences: count}, nil
}

// MultiEdit applies edits to the file at p all or nothing.
func (b *Backend) MultiEdit(_ context.Context, p string, edits []backend.EditOp) (backend.EditResult, error) {
	p, err := backend.Clean(p)
	if err != nil {
		return backend.EditResult{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := b.load("multi-edit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	updated, count, err := backend.ApplyEdits(f.content, edits)
	if err != nil {
		return backend.EditResult{}, fmt.Errorf("backend/memory: multi-edit %s: %w", p, err)
	}
	b.files[p] = file{content: updated, modTime: b.now()}
	return backend.EditResult{Occurrences: count}, nil
}

// Delete removes the file at p, or every file beneath the directory p.
func (b *Backend) Delete(_ context.Context, p string) error {
	p, err := backend.Clean(p)
	if err != nil {
		return err
	}
	if p == rootPath {
		return fmt.Errorf("backend/memory: delete %s: %w: cannot delete the root", p, backend.ErrInvalidPath)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.kindOf(p) {
	case missing:
		return fail("delete", p, backend.ErrNotFound)
	case isFile:
		delete(b.files, p)
		return nil
	}
	prefix := p + "/"
	for name := range b.files {
		if strings.HasPrefix(name, prefix) {
			delete(b.files, name)
		}
	}
	return nil
}

// Glob returns the files under base matching pattern, newest first. A relative
// pattern is resolved against base; an empty base means the root.
func (b *Backend) Glob(_ context.Context, pattern, base string) ([]backend.FileInfo, error) {
	base, err := cleanBase(base)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = path.Join(base, pattern)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if err := b.requireDir("glob", base); err != nil {
		return nil, err
	}
	var out []backend.FileInfo
	for _, name := range b.filesUnder(base) {
		if backend.MatchGlob(pattern, name) {
			f := b.files[name]
			out = append(out, backend.FileInfo{Path: name, Size: int64(len(f.content)), ModTime: f.modTime})
		}
	}
	backend.SortGlobResults(out)
	return out, nil
}

// Grep searches file contents under opts.Path.
func (b *Backend) Grep(_ context.Context, pattern string, opts backend.GrepOptions) ([]backend.GrepMatch, error) {
	base, err := cleanBase(opts.Path)
	if err != nil {
		return nil, err
	}
	grepper, err := backend.NewGrepper(pattern, opts, base)
	if err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	var candidates []string
	if _, isSingleFile := b.files[base]; isSingleFile {
		candidates = []string{base}
	} else if err := b.requireDir("grep", base); err != nil {
		return nil, err
	} else {
		candidates = b.filesUnder(base)
	}
	for _, name := range candidates {
		if !grepper.WantsPath(name) {
			continue
		}
		if grepper.AddFile(name, b.files[name].content) {
			break
		}
	}
	return grepper.Results(), nil
}

func cleanBase(p string) (string, error) {
	if p == "" {
		return rootPath, nil
	}
	return backend.Clean(p)
}

// requireDir fails unless p is an existing directory. Callers must hold b.mu.
func (b *Backend) requireDir(op, p string) error {
	switch b.kindOf(p) {
	case missing:
		return fail(op, p, backend.ErrNotFound)
	case isFile:
		return fail(op, p, backend.ErrNotDir)
	}
	return nil
}

// filesUnder returns the files below directory dir in directory-walk order.
func (b *Backend) filesUnder(dir string) []string {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var names []string
	for name := range b.files {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, backend.ComparePaths)
	return names
}

type snapshotFile struct {
	Content []byte    `json:"content"` // encoded as base64 by encoding/json
	ModTime time.Time `json:"mod_time"`
}

type snapshot struct {
	Files map[string]snapshotFile `json:"files"`
}

// Snapshot captures every file as JSON. It returns ctx.Err() when ctx has
// already ended.
func (b *Backend) Snapshot(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	snap := snapshot{Files: make(map[string]snapshotFile, len(b.files))}
	for name, f := range b.files {
		snap.Files[name] = snapshotFile{Content: f.content, ModTime: f.modTime}
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("backend/memory: snapshot: %w", err)
	}
	return data, nil
}

// Restore replaces the backend's content with a value produced by Snapshot.
// It returns ctx.Err(), changing nothing, when ctx has already ended.
func (b *Backend) Restore(ctx context.Context, data json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("backend/memory: restore: %w", err)
	}
	files := make(map[string]file, len(snap.Files))
	for name, f := range snap.Files {
		clean, err := backend.Clean(name)
		if err != nil || clean != name || clean == rootPath {
			return fmt.Errorf("backend/memory: restore: bad path %q", name)
		}
		files[name] = file{content: f.Content, modTime: f.ModTime}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.files = files
	return nil
}
