package composite_test

import (
	"context"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
)

// mntAdapter presents a backend mounted at /mnt as if it were the root: every
// path, base and absolute pattern is prefixed with /mnt and results are
// stripped of it. It proves the routed side passes the conformance suite
// without the synthetic /mnt entry showing up at the root.
type mntAdapter struct{ b backend.Backend }

const mnt = "/mnt"

func in(p string) string {
	if p == "" {
		return p // keep the invalid-path case invalid
	}
	c, err := backend.Clean(p)
	if err != nil {
		return p
	}
	return strings.TrimSuffix(mnt+c, "/")
}

func inPattern(p string) string {
	if strings.HasPrefix(p, "/") {
		return mnt + p
	}
	return p
}

func out(p string) string {
	if p == mnt {
		return "/"
	}
	return strings.TrimPrefix(p, mnt)
}

func (a mntAdapter) Ls(ctx context.Context, p string) ([]backend.FileInfo, error) {
	infos, err := a.b.Ls(ctx, in(p))
	for i := range infos {
		infos[i].Path = out(infos[i].Path)
	}
	return infos, err
}

func (a mntAdapter) Read(ctx context.Context, p string, offset, limit int) (backend.ReadResult, error) {
	return a.b.Read(ctx, in(p), offset, limit)
}

func (a mntAdapter) Write(ctx context.Context, p string, content []byte) error {
	return a.b.Write(ctx, in(p), content)
}

func (a mntAdapter) Edit(ctx context.Context, p, old, repl string, all bool) (backend.EditResult, error) {
	return a.b.Edit(ctx, in(p), old, repl, all)
}

func (a mntAdapter) MultiEdit(ctx context.Context, p string, edits []backend.EditOp) (backend.EditResult, error) {
	return a.b.(backend.MultiEditor).MultiEdit(ctx, in(p), edits)
}

func (a mntAdapter) Delete(ctx context.Context, p string) error { return a.b.Delete(ctx, in(p)) }

func (a mntAdapter) Glob(ctx context.Context, pattern, base string) ([]backend.FileInfo, error) {
	if base == "" {
		base = "/"
	}
	infos, err := a.b.Glob(ctx, inPattern(pattern), in(base))
	for i := range infos {
		infos[i].Path = out(infos[i].Path)
	}
	return infos, err
}

func (a mntAdapter) Grep(ctx context.Context, pattern string, opts backend.GrepOptions) ([]backend.GrepMatch, error) {
	if opts.Path == "" {
		opts.Path = "/"
	}
	opts.Path, opts.Glob = in(opts.Path), inPattern(opts.Glob)
	ms, err := a.b.Grep(ctx, pattern, opts)
	for i := range ms {
		ms[i].Path = out(ms[i].Path)
	}
	return ms, err
}
