package kv

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/aswin-kevin/orchestrix/backend"
)

const rootPath = "/"

// Backend is a backend.Backend (and backend.MultiEditor) over one namespace of
// a Store. Directories are implicit, as in backend/memory. Read-modify-write
// operations are serialized within one Backend value; share one value per
// namespace in a process. Across processes the last write wins.
type Backend struct {
	store     Store
	namespace string
	mu        sync.RWMutex
}

var (
	_ backend.Backend     = (*Backend)(nil)
	_ backend.MultiEditor = (*Backend)(nil)
)

// New returns a Backend over namespace of store. A nil store, an empty
// namespace, one longer than MaxNamespaceBytes, or one containing a NUL byte
// is an error.
func New(store Store, namespace string) (*Backend, error) {
	if store == nil {
		return nil, errors.New("kv: nil store")
	}
	if err := checkNamespace(namespace); err != nil {
		return nil, err
	}
	return &Backend{store: store, namespace: namespace}, nil
}

func fail(op, p string, sentinel error) error {
	return fmt.Errorf("kv: %s %s: %w", op, p, sentinel)
}

// wrap adds the operation and path to a store error, naming the package only
// when the error does not already.
func wrap(op, p string, err error) error {
	if strings.HasPrefix(err.Error(), "kv:") {
		return fmt.Errorf("%s %s: %w", op, p, err)
	}
	return fmt.Errorf("kv: %s %s: %w", op, p, err)
}

// dirPrefix returns the key prefix shared by everything below directory dir.
func dirPrefix(dir string) string {
	return strings.TrimSuffix(dir, "/") + "/"
}

func cleanBase(p string) (string, error) {
	if p == "" {
		return rootPath, nil
	}
	return backend.Clean(p)
}

// lookup fetches the file at p; ok is false when there is none.
func (b *Backend) lookup(ctx context.Context, p string) (item Item, ok bool, err error) {
	item, err = b.store.Get(ctx, b.namespace, p)
	if errors.Is(err, backend.ErrNotFound) {
		return Item{}, false, nil
	}
	return item, err == nil, err
}

// stat returns the metadata of the file at p without reading its value; ok is
// false when there is none.
func (b *Backend) stat(ctx context.Context, p string) (info ItemInfo, ok bool, err error) {
	if s, fast := b.store.(stater); fast {
		info, err = s.stat(ctx, b.namespace, p)
	} else {
		var item Item
		if item, err = b.store.Get(ctx, b.namespace, p); err == nil {
			info = ItemInfo{Key: p, Size: int64(len(item.Value)), ModTime: item.ModTime}
		}
	}
	if errors.Is(err, backend.ErrNotFound) {
		return ItemInfo{}, false, nil
	}
	return info, err == nil, err
}

// below lists the files beneath directory dir.
func (b *Backend) below(ctx context.Context, dir string) ([]ItemInfo, error) {
	infos, err := b.store.List(ctx, b.namespace, dirPrefix(dir))
	if errors.Is(err, ErrKeyTooLong) {
		// No key that long was ever stored, so nothing lies beneath dir.
		return nil, nil
	}
	return infos, err
}

// kind classifies a cleaned path.
type kind int

const (
	missing kind = iota
	isFile
	isDir
)

func (b *Backend) kindOf(ctx context.Context, p string) (kind, error) {
	if _, ok, err := b.stat(ctx, p); err != nil || ok {
		return isFile, err
	}
	if p == rootPath {
		return isDir, nil
	}
	infos, err := b.below(ctx, p)
	if err != nil || len(infos) == 0 {
		return missing, err
	}
	return isDir, nil
}

// requireDir fails unless p is an existing directory (the root always is).
func (b *Backend) requireDir(ctx context.Context, op, p string) error {
	k, err := b.kindOf(ctx, p)
	if err != nil {
		return wrap(op, p, err)
	}
	switch k {
	case missing:
		return fail(op, p, backend.ErrNotFound)
	case isFile:
		return fail(op, p, backend.ErrNotDir)
	}
	return nil
}

// hasFileAncestor reports whether some proper ancestor of p is a file.
func (b *Backend) hasFileAncestor(ctx context.Context, p string) (bool, error) {
	for dir := path.Dir(p); dir != rootPath; dir = path.Dir(dir) {
		if _, ok, err := b.stat(ctx, dir); err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// Ls lists the direct children of the directory at p, sorted by name.
func (b *Backend) Ls(ctx context.Context, p string) ([]backend.FileInfo, error) {
	p, err := backend.Clean(p)
	if err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if _, ok, err := b.stat(ctx, p); err != nil {
		return nil, wrap("ls", p, err)
	} else if ok {
		return nil, fail("ls", p, backend.ErrNotDir)
	}
	infos, err := b.below(ctx, p)
	if err != nil {
		return nil, wrap("ls", p, err)
	}
	if len(infos) == 0 && p != rootPath {
		return nil, fail("ls", p, backend.ErrNotFound)
	}
	return children(p, infos), nil
}

// children folds the files below dir into its direct entries, sorted by path.
func children(dir string, infos []ItemInfo) []backend.FileInfo {
	prefix := dirPrefix(dir)
	entries := map[string]backend.FileInfo{}
	for _, f := range infos {
		rest, ok := strings.CutPrefix(f.Key, prefix)
		if !ok {
			continue
		}
		child, _, nested := strings.Cut(rest, "/")
		info := backend.FileInfo{Path: prefix + child, IsDir: nested, ModTime: f.ModTime}
		if nested {
			if prev, seen := entries[info.Path]; seen && prev.ModTime.After(f.ModTime) {
				info.ModTime = prev.ModTime
			}
		} else {
			info.Size = f.Size
		}
		entries[info.Path] = info
	}
	out := make([]backend.FileInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, c backend.FileInfo) int { return strings.Compare(a.Path, c.Path) })
	return out
}

// load returns the file at the cleaned path p for an operation that needs its
// whole content: it fails for a directory, a missing file, or a file over
// backend.MaxReadFileBytes. Callers must hold b.mu.
func (b *Backend) load(ctx context.Context, op, p string) (Item, error) {
	info, ok, err := b.stat(ctx, p)
	if err != nil {
		return Item{}, wrap(op, p, err)
	}
	if !ok {
		k, err := b.kindOf(ctx, p)
		if err != nil {
			return Item{}, wrap(op, p, err)
		}
		if k == isDir {
			return Item{}, fail(op, p, backend.ErrIsDir)
		}
		return Item{}, fail(op, p, backend.ErrNotFound)
	}
	if err := backend.CheckReadSize(info.Size); err != nil {
		return Item{}, fail(op, p, err)
	}
	item, err := b.store.Get(ctx, b.namespace, p)
	if err != nil {
		return Item{}, wrap(op, p, err)
	}
	return item, nil
}

// Read returns a page of the file at p.
func (b *Backend) Read(ctx context.Context, p string, offset, limit int) (backend.ReadResult, error) {
	p, err := backend.Clean(p)
	if err != nil {
		return backend.ReadResult{}, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	item, err := b.load(ctx, "read", p)
	if err != nil {
		return backend.ReadResult{}, err
	}
	return backend.NewReadResult(item.Value, item.ModTime, offset, limit), nil
}

// Write replaces the file at p, creating parent directories implicitly.
func (b *Backend) Write(ctx context.Context, p string, content []byte) error {
	p, err := backend.Clean(p)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	k := isDir
	if p != rootPath {
		if k, err = b.kindOf(ctx, p); err != nil {
			return wrap("write", p, err)
		}
	}
	if k == isDir {
		return fail("write", p, backend.ErrIsDir)
	}
	if blocked, err := b.hasFileAncestor(ctx, p); err != nil {
		return wrap("write", p, err)
	} else if blocked {
		return fail("write", p, backend.ErrNotDir)
	}
	if err := b.store.Put(ctx, b.namespace, p, content); err != nil {
		return wrap("write", p, err)
	}
	return nil
}

// Edit replaces old with replacement in the file at p.
func (b *Backend) Edit(ctx context.Context, p, old, replacement string, replaceAll bool) (backend.EditResult, error) {
	return b.edit(ctx, "edit", p, func(content []byte) ([]byte, int, error) {
		return backend.ApplyEdit(content, old, replacement, replaceAll)
	})
}

// MultiEdit applies edits to the file at p all or nothing.
func (b *Backend) MultiEdit(ctx context.Context, p string, edits []backend.EditOp) (backend.EditResult, error) {
	return b.edit(ctx, "multi-edit", p, func(content []byte) ([]byte, int, error) {
		return backend.ApplyEdits(content, edits)
	})
}

func (b *Backend) edit(ctx context.Context, op, p string, apply func([]byte) ([]byte, int, error)) (backend.EditResult, error) {
	p, err := backend.Clean(p)
	if err != nil {
		return backend.EditResult{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	item, err := b.load(ctx, op, p)
	if err != nil {
		return backend.EditResult{}, err
	}
	updated, count, err := apply(item.Value)
	if err != nil {
		return backend.EditResult{}, wrap(op, p, err)
	}
	if err := b.store.Put(ctx, b.namespace, p, updated); err != nil {
		return backend.EditResult{}, wrap(op, p, err)
	}
	return backend.EditResult{Occurrences: count}, nil
}

// Delete removes the file at p, or every file beneath the directory p.
func (b *Backend) Delete(ctx context.Context, p string) error {
	p, err := backend.Clean(p)
	if err != nil {
		return err
	}
	if p == rootPath {
		return fmt.Errorf("kv: delete %s: %w: cannot delete the root", p, backend.ErrInvalidPath)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok, err := b.stat(ctx, p); err != nil {
		return wrap("delete", p, err)
	} else if ok {
		return b.removeKeys(ctx, p, []string{p})
	}
	infos, err := b.below(ctx, p)
	if err != nil {
		return wrap("delete", p, err)
	}
	if len(infos) == 0 {
		return fail("delete", p, backend.ErrNotFound)
	}
	keys := make([]string, len(infos))
	for i, info := range infos {
		keys[i] = info.Key
	}
	return b.removeKeys(ctx, p, keys)
}

func (b *Backend) removeKeys(ctx context.Context, p string, keys []string) error {
	for _, key := range keys {
		if err := b.store.Delete(ctx, b.namespace, key); err != nil {
			return wrap("delete", p, err)
		}
	}
	return nil
}

// Glob returns the files under base matching pattern, newest first. A relative
// pattern is resolved against base; an empty base means the root.
func (b *Backend) Glob(ctx context.Context, pattern, base string) ([]backend.FileInfo, error) {
	base, err := cleanBase(base)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = path.Join(base, pattern)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if err := b.requireDir(ctx, "glob", base); err != nil {
		return nil, err
	}
	infos, err := b.below(ctx, base)
	if err != nil {
		return nil, wrap("glob", base, err)
	}
	var out []backend.FileInfo
	for _, f := range infos {
		if backend.MatchGlob(pattern, f.Key) {
			out = append(out, backend.FileInfo{Path: f.Key, Size: f.Size, ModTime: f.ModTime})
		}
	}
	backend.SortGlobResults(out)
	return out, nil
}

// Grep searches file contents under opts.Path. Files over
// backend.MaxGrepFileBytes and binary files are skipped.
func (b *Backend) Grep(ctx context.Context, pattern string, opts backend.GrepOptions) ([]backend.GrepMatch, error) {
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
	var candidates []ItemInfo
	if info, ok, err := b.stat(ctx, base); err != nil {
		return nil, wrap("grep", base, err)
	} else if ok {
		candidates = []ItemInfo{info}
	} else if err := b.requireDir(ctx, "grep", base); err != nil {
		return nil, err
	} else if candidates, err = b.below(ctx, base); err != nil {
		return nil, wrap("grep", base, err)
	}
	slices.SortFunc(candidates, func(x, y ItemInfo) int { return backend.ComparePaths(x.Key, y.Key) })
	for _, c := range candidates {
		if c.Size > backend.MaxGrepFileBytes || !grepper.WantsPath(c.Key) {
			continue
		}
		item, ok, err := b.lookup(ctx, c.Key)
		if err != nil {
			return nil, wrap("grep", c.Key, err)
		}
		if ok && grepper.AddFile(c.Key, item.Value) {
			break
		}
	}
	return grepper.Results(), nil
}
