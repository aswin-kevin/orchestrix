// Package composite routes each path to one of several backends by its longest
// prefix and merges directory listings, globs and greps across them. Mount
// memory.DefaultDir onto a persistent backend such as backend/kv to keep
// memories across threads while the rest of the workspace stays per thread.
//
// Backend has no move or rename, so no edit ever crosses routes. Commands see
// only the default backend: mounted files are invisible to them, and def files
// a mount shadows remain visible.
//
// The composite owns its backends: its Close (see New) closes the default
// backend and every route's backend.
package composite

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aswin-kevin/orchestrix/backend"
)

// Route mounts Backend at Prefix: a path at or below Prefix is served by
// Backend with Prefix stripped ("/memories/a.md" -> "/a.md"; "/memories" ->
// "/").
type Route struct {
	// Prefix is absolute, cleaned and not "/"; a trailing "/" is accepted and
	// dropped.
	Prefix string
	// Backend serves every path at or below Prefix; it must not be nil.
	Backend backend.Backend
}

// ErrMountPoint is wrapped by Delete of a mount point or of an ancestor of one.
var ErrMountPoint = errors.New("composite: path contains a mount point")

type composite struct {
	def    backend.Backend
	mounts []mount // longest prefix first

	// The capabilities of def or a backend it wraps (see backend.Backend),
	// set by withCapabilities; nil when def has none.
	exec  backend.Executor
	start backend.Starter
	snap  backend.Snapshotter

	// owned is def followed by the route backends in the order given to
	// New: the order Close releases them in.
	owned []backend.Backend

	closeMu   sync.Mutex
	closing   bool          // the first Close has started
	closeDone chan struct{} // closed once the first Close has finished
	closeErr  error         // written before closeDone is closed
}

var (
	_ backend.Backend     = (*composite)(nil)
	_ backend.MultiEditor = (*composite)(nil)
	_ backend.Closer      = (*composite)(nil)
)

// New returns a backend that routes each path to the route with the longest
// Prefix that equals it or is a whole-segment prefix of it, else to def. The
// result also implements backend.MultiEditor (delegating; a target without it
// fails with an error wrapping errors.ErrUnsupported); it implements
// backend.Executor and backend.Starter exactly when def implements both,
// backend.Executor alone when def implements only that, and
// backend.Snapshotter when def does (snapshots cover def only: routes persist
// on their own). A capability of a backend def wraps counts as def's (see
// backend.Backend's Unwrap). Commands run in def; ExecOptions.Dir inside a
// mount fails with an error wrapping errors.ErrUnsupported.
//
// The result always implements backend.Closer. Close closes def first, then
// each route's backend in the order given here, each through
// backend.AsCloser; a backend given more than once (the same comparable
// Closer, also when reached through Unwrap) is closed once, and the errors
// are joined. The first call does the work; later and concurrent calls
// return its result. Do not close the composite while a backend it was
// given must stay open for someone else.
//
// New returns backend.Backend rather than a concrete type because the
// optional capabilities of the result depend on def.
// Errors: nil def or route backend, invalid or duplicate prefix.
func New(def backend.Backend, routes ...Route) (backend.Backend, error) {
	if def == nil {
		return nil, errors.New("composite: nil default backend")
	}
	mounts, err := checkRoutes(routes)
	if err != nil {
		return nil, err
	}
	owned := []backend.Backend{def}
	for _, r := range routes {
		owned = append(owned, r.Backend)
	}
	return withCapabilities(&composite{def: def, mounts: mounts, owned: owned}), nil
}

// target picks the backend serving p and the path to give it. A path that is a
// proper ancestor of a mount is a directory the default backend may not have,
// so file operations on it fail with ErrIsDir.
func (c *composite) target(op, p string) (backend.Backend, string, error) {
	p, err := backend.Clean(p)
	if err != nil {
		return nil, "", err
	}
	if c.isAncestor(p) {
		return nil, "", fmt.Errorf("composite: %s %s: %w", op, p, backend.ErrIsDir)
	}
	if m, rel, ok := c.resolve(p); ok {
		return m.backend, rel, nil
	}
	return c.def, p, nil
}

// Ls lists a directory; see the package merging rules: entries of the default
// backend at or below a mount are hidden and each mount below p shows up as a
// directory.
func (c *composite) Ls(ctx context.Context, p string) ([]backend.FileInfo, error) {
	p, err := backend.Clean(p)
	if err != nil {
		return nil, err
	}
	return c.mergeLs(ctx, p)
}

// Read reads from the backend serving path.
func (c *composite) Read(ctx context.Context, p string, offset, limit int) (backend.ReadResult, error) {
	b, rel, err := c.target("read", p)
	if err != nil {
		return backend.ReadResult{}, err
	}
	return b.Read(ctx, rel, offset, limit)
}

// Write writes to the backend serving path.
func (c *composite) Write(ctx context.Context, p string, content []byte) error {
	b, rel, err := c.target("write", p)
	if err != nil {
		return err
	}
	return b.Write(ctx, rel, content)
}

// Edit edits in the backend serving path.
func (c *composite) Edit(ctx context.Context, p, old, replacement string, replaceAll bool) (backend.EditResult, error) {
	b, rel, err := c.target("edit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	return b.Edit(ctx, rel, old, replacement, replaceAll)
}

// MultiEdit applies edits in the backend serving path; that backend must
// implement backend.MultiEditor.
func (c *composite) MultiEdit(ctx context.Context, p string, edits []backend.EditOp) (backend.EditResult, error) {
	b, rel, err := c.target("multiedit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	me, ok := backend.AsMultiEditor(b)
	if !ok {
		return backend.EditResult{}, fmt.Errorf("composite: multi-edit %s: %w", p, errors.ErrUnsupported)
	}
	return me.MultiEdit(ctx, rel, edits)
}

// Delete removes a file or directory. A mount point, or any ancestor of one
// (including the root), is refused with ErrMountPoint and nothing is deleted.
func (c *composite) Delete(ctx context.Context, p string) error {
	p, err := backend.Clean(p)
	if err != nil {
		return err
	}
	if c.isAncestor(p) {
		return fmt.Errorf("composite: delete %s: %w", p, ErrMountPoint)
	}
	if m, rel, ok := c.resolve(p); ok {
		if rel == "/" {
			return fmt.Errorf("composite: delete %s: %w", p, ErrMountPoint)
		}
		return m.backend.Delete(ctx, rel)
	}
	return c.def.Delete(ctx, p)
}
