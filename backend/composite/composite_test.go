package composite_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/composite"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	memmw "github.com/aswin-kevin/orchestrix/middleware/memory"
)

var ctx = context.Background()

func mustNew(t *testing.T, def backend.Backend, routes ...composite.Route) backend.Backend {
	t.Helper()
	b, err := composite.New(def, routes...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func put(t *testing.T, b backend.Backend, p, content string) {
	t.Helper()
	if err := b.Write(ctx, p, []byte(content)); err != nil {
		t.Fatalf("Write(%q): %v", p, err)
	}
}

func readAll(t *testing.T, b backend.Backend, p string) string {
	t.Helper()
	res, err := b.Read(ctx, p, 0, 0)
	if err != nil {
		t.Fatalf("Read(%q): %v", p, err)
	}
	return res.Lines[0]
}

func has(b backend.Backend, p string) bool {
	_, err := b.Read(ctx, p, 0, 0)
	return err == nil
}

func paths(infos []backend.FileInfo) []string {
	var out []string
	for _, fi := range infos {
		out = append(out, fi.Path)
	}
	return out
}

func wantPaths(t *testing.T, what string, got []backend.FileInfo, want ...string) {
	t.Helper()
	if !slices.Equal(paths(got), want) {
		t.Fatalf("%s = %q, want %q", what, paths(got), want)
	}
}

func TestCompositeConformance(t *testing.T) {
	t.Run("NoRoutes", func(t *testing.T) {
		backendtest.Run(t, func(t *testing.T) backend.Backend {
			return mustNew(t, memory.New())
		})
	})
	t.Run("ThroughMount", func(t *testing.T) {
		backendtest.Run(t, func(t *testing.T) backend.Backend {
			return mntAdapter{mustNew(t, memory.New(), composite.Route{Prefix: mnt, Backend: memory.New()})}
		})
	})
}

func TestRoutesByLongestPrefix(t *testing.T) {
	a, b := memory.New(), memory.New()
	c := mustNew(t, memory.New(), composite.Route{Prefix: "/m", Backend: a}, composite.Route{Prefix: "/m/deep", Backend: b})
	put(t, c, "/m/deep/x", "in-b")
	put(t, c, "/m/y", "in-a")
	if got := readAll(t, b, "/x"); got != "in-b" {
		t.Fatalf("B /x = %q", got)
	}
	if has(a, "/deep/x") || has(a, "/x") {
		t.Fatal("deep write leaked into A")
	}
	if got := readAll(t, a, "/y"); got != "in-a" {
		t.Fatalf("A /y = %q", got)
	}
	if readAll(t, c, "/m/deep/x") != "in-b" || readAll(t, c, "/m/y") != "in-a" {
		t.Fatal("read back through composite")
	}
	infos, err := c.Ls(ctx, "/m")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "Ls(/m)", infos, "/m/deep", "/m/y")

	// A's own /deep/s.md is shadowed by the deeper mount B everywhere.
	put(t, a, "/deep/s.md", "shadow")
	put(t, c, "/m/deep/t.md", "shadow")
	globbed, err := c.Glob(ctx, "**/*.md", "/m")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "Glob(/m)", globbed, "/m/deep/t.md")
	ms, err := c.Grep(ctx, "shadow", backend.GrepOptions{Path: "/m"})
	if err != nil || len(ms) != 1 || ms[0].Path != "/m/deep/t.md" {
		t.Fatalf("Grep(/m) = %+v, %v", ms, err)
	}
}

func TestPrefixBoundary(t *testing.T) {
	def, route := memory.New(), memory.New()
	c := mustNew(t, def, composite.Route{Prefix: "/memories", Backend: route})
	put(t, c, "/memories2/x", "1")
	put(t, c, "/memoriesX", "2")
	put(t, c, "/memories/ok", "3")
	if !has(def, "/memories2/x") || !has(def, "/memoriesX") {
		t.Fatal("sibling-prefix paths must land in the default backend")
	}
	if has(route, "/2/x") || has(route, "/X") || has(route, "/memories2/x") {
		t.Fatal("sibling-prefix path leaked into the route")
	}
	if !has(route, "/ok") || has(def, "/memories/ok") {
		t.Fatal("/memories/ok must land only in the route")
	}
	if _, err := c.Read(ctx, "/memories2/x", 0, 0); err != nil {
		t.Fatal(err)
	}

	slash := memory.New()
	c2 := mustNew(t, memory.New(), composite.Route{Prefix: "/memories/", Backend: slash})
	put(t, c2, "/memories/a", "x")
	if !has(slash, "/a") {
		t.Fatal("trailing slash prefix should behave like /memories")
	}

	for _, prefix := range []string{"/", "", "rel", "/a/../b", "/a//b", "/a\x00b"} {
		if _, err := composite.New(memory.New(), composite.Route{Prefix: prefix, Backend: memory.New()}); err == nil {
			t.Errorf("prefix %q accepted", prefix)
		}
	}
	dup := []composite.Route{{Prefix: "/a", Backend: memory.New()}, {Prefix: "/a/", Backend: memory.New()}}
	if _, err := composite.New(memory.New(), dup...); err == nil {
		t.Error("duplicate prefix accepted")
	}
	if _, err := composite.New(nil); err == nil {
		t.Error("nil default accepted")
	}
	if _, err := composite.New(memory.New(), composite.Route{Prefix: "/a"}); err == nil {
		t.Error("nil route backend accepted")
	}
}

func TestLsShowsMountAncestors(t *testing.T) {
	c := mustNew(t, memory.New(), composite.Route{Prefix: memmw.DefaultDir, Backend: memory.New()})
	got, err := c.Ls(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/.orchestrix" || !got[0].IsDir {
		t.Fatalf("Ls(/) = %+v", got)
	}
	got, err = c.Ls(ctx, "/.orchestrix")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/.orchestrix/memories" || !got[0].IsDir {
		t.Fatalf("Ls(/.orchestrix) = %+v", got)
	}
	if _, err := c.Ls(ctx, "/nope"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Ls(/nope) = %v, want ErrNotFound", err)
	}
	// Mount ancestors are directories, not files.
	if _, err := c.Read(ctx, "/.orchestrix", 0, 0); !errors.Is(err, backend.ErrIsDir) {
		t.Fatalf("Read(ancestor) = %v, want ErrIsDir", err)
	}
	if err := c.Write(ctx, "/.orchestrix", []byte("x")); !errors.Is(err, backend.ErrIsDir) {
		t.Fatalf("Write(ancestor) = %v, want ErrIsDir", err)
	}
}

func TestDeleteMountRefused(t *testing.T) {
	route := memory.New()
	def := memory.New()
	c := mustNew(t, def, composite.Route{Prefix: "/a/memories", Backend: route})
	put(t, c, "/a/memories/b.md", "b")
	put(t, c, "/a/keep.txt", "k")
	for _, p := range []string{"/a/memories", "/a", "/"} {
		if err := c.Delete(ctx, p); !errors.Is(err, composite.ErrMountPoint) {
			t.Fatalf("Delete(%q) = %v, want ErrMountPoint", p, err)
		}
	}
	if !has(c, "/a/memories/b.md") || !has(c, "/a/keep.txt") {
		t.Fatal("refused delete removed something")
	}
	if err := c.Delete(ctx, "/a/memories/b.md"); err != nil || has(route, "/b.md") {
		t.Fatalf("Delete inside mount: %v", err)
	}
	if err := c.Delete(ctx, "/a/keep.txt"); err != nil {
		t.Fatal(err)
	}
	// A sibling that merely shares the prefix text is an ordinary path.
	put(t, c, "/a/memories2/x", "x")
	if err := c.Delete(ctx, "/a/memories2"); err != nil {
		t.Fatalf("Delete(/a/memories2) = %v", err)
	}
}

func TestEditAndMultiEditRoute(t *testing.T) {
	route := memory.New()
	c := mustNew(t, memory.New(), composite.Route{Prefix: "/m", Backend: route})
	put(t, c, "/m/f", "one two")
	if _, err := c.Edit(ctx, "/m/f", "one", "1", false); err != nil {
		t.Fatal(err)
	}
	me := c.(backend.MultiEditor)
	if _, err := me.MultiEdit(ctx, "/m/f", []backend.EditOp{{Old: "two", New: "2"}}); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, route, "/f"); got != "1 2" {
		t.Fatalf("content = %q", got)
	}
	// A target without MultiEdit fails with ErrUnsupported.
	c2 := mustNew(t, memory.New(), composite.Route{Prefix: "/m", Backend: noMulti{memory.New()}})
	put(t, c2, "/m/f", "x")
	if _, err := c2.(backend.MultiEditor).MultiEdit(ctx, "/m/f", []backend.EditOp{{Old: "x", New: "y"}}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("MultiEdit = %v, want ErrUnsupported", err)
	}
}

// noMulti hides the embedded backend's MultiEdit.
type noMulti struct{ backend.Backend }

func TestCapabilitiesFollowDefault(t *testing.T) {
	testutil.RequireUnixShell(t)
	sh, err := shell.New(t.TempDir(), shell.Config{})
	if err != nil {
		t.Fatal(err)
	}
	c := mustNew(t, sh, composite.Route{Prefix: "/memories", Backend: memory.New()})
	ex, ok := c.(backend.Executor)
	if _, st := c.(backend.Starter); !ok || !st {
		t.Fatal("shell default must yield Executor and Starter")
	}
	res, err := ex.Execute(ctx, "echo hi", backend.ExecOptions{})
	if err != nil || res.Output != "hi\n" {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	if _, err := ex.Execute(ctx, "true", backend.ExecOptions{Dir: "/memories/x"}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Execute in mount = %v, want ErrUnsupported", err)
	}
	if _, err := c.(backend.Starter).Start(ctx, "true", backend.ExecOptions{Dir: "/memories"}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Start in mount = %v, want ErrUnsupported", err)
	}
	if _, snap := c.(backend.Snapshotter); snap {
		t.Fatal("shell default is not a Snapshotter")
	}
	// An Executor-only default yields Executor without Starter.
	only := mustNew(t, execOnly{memory.New()})
	if _, ok := only.(backend.Executor); !ok {
		t.Fatal("Executor-only default lost Executor")
	}
	if _, ok := only.(backend.Starter); ok {
		t.Fatal("Executor-only default gained Starter")
	}
	if _, ok := only.(backend.Snapshotter); ok {
		t.Fatal("execOnly is not a Snapshotter")
	}
}

type execOnly struct{ backend.Backend }

func (execOnly) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	return backend.ExecResult{}, nil
}

func TestSnapshotCoversDefaultOnly(t *testing.T) {
	route := memory.New()
	c := mustNew(t, memory.New(), composite.Route{Prefix: "/memories", Backend: route})
	if _, ok := c.(backend.Executor); ok {
		t.Fatal("memory default must not yield Executor")
	}
	snap, ok := c.(backend.Snapshotter)
	if !ok {
		t.Fatal("memory default must yield Snapshotter")
	}
	put(t, c, "/scratch.txt", "before")
	put(t, c, "/memories/m.md", "kept")
	data, err := snap.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, "/scratch.txt", "after")
	put(t, c, "/extra.txt", "new")
	put(t, c, "/memories/m.md", "changed")
	if err := snap.Restore(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	if readAll(t, c, "/scratch.txt") != "before" || has(c, "/extra.txt") {
		t.Fatal("def files not restored")
	}
	if got := readAll(t, c, "/memories/m.md"); got != "changed" {
		t.Fatalf("route file = %q, want untouched \"changed\"", got)
	}
}

func TestMemoriesPersistAcrossThreads(t *testing.T) {
	shared, err := kv.New(kv.NewMemoryStore(), "memories")
	if err != nil {
		t.Fatal(err)
	}
	thread := func() backend.Backend {
		return mustNew(t, memory.New(), composite.Route{Prefix: memmw.DefaultDir, Backend: shared})
	}
	a, b := thread(), thread()
	put(t, a, memmw.DefaultDir+"/color.md", "blue")
	put(t, a, "/scratch.txt", "mine")
	if got := readAll(t, b, memmw.DefaultDir+"/color.md"); got != "blue" {
		t.Fatalf("thread B memory = %q", got)
	}
	if has(b, "/scratch.txt") {
		t.Fatal("scratch file leaked across threads")
	}
}

// unwrapping hides every optional capability of the backend it wraps but
// exposes it through Unwrap.
type unwrapping struct{ backend.Backend }

func (u unwrapping) Unwrap() backend.Backend { return u.Backend }

func TestCapabilitiesThroughUnwrap(t *testing.T) {
	c := mustNew(t, unwrapping{memory.New()}, composite.Route{Prefix: "/m", Backend: unwrapping{memory.New()}})
	if _, ok := c.(backend.Snapshotter); !ok {
		t.Fatal("default wrapping a Snapshotter must yield Snapshotter")
	}
	put(t, c, "/m/f", "x")
	if _, err := c.(backend.MultiEditor).MultiEdit(ctx, "/m/f", []backend.EditOp{{Old: "x", New: "y"}}); err != nil {
		t.Fatalf("MultiEdit through a route's Unwrap = %v", err)
	}
	ex, ok := mustNew(t, unwrapping{execOnly{memory.New()}}).(backend.Executor)
	if !ok {
		t.Fatal("default wrapping an Executor must yield Executor")
	}
	if _, err := ex.Execute(ctx, "true", backend.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
}
