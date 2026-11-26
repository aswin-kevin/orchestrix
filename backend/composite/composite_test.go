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
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	memmw "github.com/aswin-kevin/orchestrix/middleware/memory"
)

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
	ctx := t.Context()
	if err := b.Write(ctx, p, []byte(content)); err != nil {
		t.Fatalf("Write(%q): %v", p, err)
	}
}

func readAll(t *testing.T, b backend.Backend, p string) string {
	t.Helper()
	ctx := t.Context()
	res, err := b.Read(ctx, p, 0, 0)
	if err != nil {
		t.Fatalf("Read(%q): %v", p, err)
	}
	return res.Lines[0]
}

func has(t *testing.T, b backend.Backend, p string) bool {
	t.Helper()
	ctx := t.Context()
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
			return mustNew(t, testutil.Memory(t))
		})
	})
	t.Run("ThroughMount", func(t *testing.T) {
		backendtest.Run(t, func(t *testing.T) backend.Backend {
			return mntAdapter{mustNew(t, testutil.Memory(t), composite.Route{Prefix: mnt, Backend: testutil.Memory(t)})}
		})
	})
}

func TestRoutesByLongestPrefix(t *testing.T) {
	ctx := t.Context()
	a, b := testutil.Memory(t), testutil.Memory(t)
	c := mustNew(t, testutil.Memory(t), composite.Route{Prefix: "/m", Backend: a}, composite.Route{Prefix: "/m/deep", Backend: b})
	put(t, c, "/m/deep/x", "in-b")
	put(t, c, "/m/y", "in-a")
	if got := readAll(t, b, "/x"); got != "in-b" {
		t.Fatalf("B /x = %q", got)
	}
	if has(t, a, "/deep/x") || has(t, a, "/x") {
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
	ctx := t.Context()
	def, route := testutil.Memory(t), testutil.Memory(t)
	c := mustNew(t, def, composite.Route{Prefix: "/memories", Backend: route})
	put(t, c, "/memories2/x", "1")
	put(t, c, "/memoriesX", "2")
	put(t, c, "/memories/ok", "3")
	if !has(t, def, "/memories2/x") || !has(t, def, "/memoriesX") {
		t.Fatal("sibling-prefix paths must land in the default backend")
	}
	if has(t, route, "/2/x") || has(t, route, "/X") || has(t, route, "/memories2/x") {
		t.Fatal("sibling-prefix path leaked into the route")
	}
	if !has(t, route, "/ok") || has(t, def, "/memories/ok") {
		t.Fatal("/memories/ok must land only in the route")
	}
	if _, err := c.Read(ctx, "/memories2/x", 0, 0); err != nil {
		t.Fatal(err)
	}

	slash := testutil.Memory(t)
	c2 := mustNew(t, testutil.Memory(t), composite.Route{Prefix: "/memories/", Backend: slash})
	put(t, c2, "/memories/a", "x")
	if !has(t, slash, "/a") {
		t.Fatal("trailing slash prefix should behave like /memories")
	}

	for _, prefix := range []string{"/", "", "rel", "/a/../b", "/a//b", "/a\x00b"} {
		if _, err := composite.New(testutil.Memory(t), composite.Route{Prefix: prefix, Backend: testutil.Memory(t)}); err == nil {
			t.Errorf("prefix %q accepted", prefix)
		}
	}
	dup := []composite.Route{{Prefix: "/a", Backend: testutil.Memory(t)}, {Prefix: "/a/", Backend: testutil.Memory(t)}}
	if _, err := composite.New(testutil.Memory(t), dup...); err == nil {
		t.Error("duplicate prefix accepted")
	}
	if _, err := composite.New(nil); err == nil {
		t.Error("nil default accepted")
	}
	if _, err := composite.New(testutil.Memory(t), composite.Route{Prefix: "/a"}); err == nil {
		t.Error("nil route backend accepted")
	}
}

func TestLsShowsMountAncestors(t *testing.T) {
	ctx := t.Context()
	c := mustNew(t, testutil.Memory(t), composite.Route{Prefix: memmw.DefaultDir, Backend: testutil.Memory(t)})
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
	ctx := t.Context()
	route := testutil.Memory(t)
	def := testutil.Memory(t)
	c := mustNew(t, def, composite.Route{Prefix: "/a/memories", Backend: route})
	put(t, c, "/a/memories/b.md", "b")
	put(t, c, "/a/keep.txt", "k")
	for _, p := range []string{"/a/memories", "/a", "/"} {
		if err := c.Delete(ctx, p); !errors.Is(err, composite.ErrMountPoint) {
			t.Fatalf("Delete(%q) = %v, want ErrMountPoint", p, err)
		}
	}
	if !has(t, c, "/a/memories/b.md") || !has(t, c, "/a/keep.txt") {
		t.Fatal("refused delete removed something")
	}
	if err := c.Delete(ctx, "/a/memories/b.md"); err != nil || has(t, route, "/b.md") {
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
	ctx := t.Context()
	route := testutil.Memory(t)
	c := mustNew(t, testutil.Memory(t), composite.Route{Prefix: "/m", Backend: route})
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
	c2 := mustNew(t, testutil.Memory(t), composite.Route{Prefix: "/m", Backend: noMulti{testutil.Memory(t)}})
	put(t, c2, "/m/f", "x")
	if _, err := c2.(backend.MultiEditor).MultiEdit(ctx, "/m/f", []backend.EditOp{{Old: "x", New: "y"}}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("MultiEdit = %v, want ErrUnsupported", err)
	}
}

// noMulti hides the embedded backend's MultiEdit.
type noMulti struct{ backend.Backend }

func TestCapabilitiesFollowDefault(t *testing.T) {
	ctx := t.Context()
	testutil.RequireUnixShell(t)
	sh, err := shell.New(t.TempDir(), shell.Config{})
	if err != nil {
		t.Fatal(err)
	}
	c := mustNew(t, sh, composite.Route{Prefix: "/memories", Backend: testutil.Memory(t)})
	ex, ok := backend.AsExecutor(c)
	st, okStart := backend.AsStarter(c)
	if !ok || !okStart {
		t.Fatal("shell default must yield Executor and Starter")
	}
	res, err := ex.Execute(ctx, "echo hi", backend.ExecOptions{})
	if err != nil || res.Output != "hi\n" {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	if _, err := ex.Execute(ctx, "true", backend.ExecOptions{Dir: "/memories/x"}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Execute in mount = %v, want ErrUnsupported", err)
	}
	if _, err := st.Start(ctx, "true", backend.ExecOptions{Dir: "/memories"}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Start in mount = %v, want ErrUnsupported", err)
	}
	if _, snap := backend.AsSnapshotter(c); snap {
		t.Fatal("shell default is not a Snapshotter")
	}
	// An Executor-only default yields Executor without Starter.
	only := mustNew(t, execOnly{testutil.Memory(t)})
	if _, ok := backend.AsExecutor(only); !ok {
		t.Fatal("Executor-only default lost Executor")
	}
	if _, ok := backend.AsStarter(only); ok {
		t.Fatal("Executor-only default gained Starter")
	}
	if _, ok := backend.AsSnapshotter(only); ok {
		t.Fatal("execOnly is not a Snapshotter")
	}
}

type execOnly struct{ backend.Backend }

func (execOnly) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	return backend.ExecResult{}, nil
}

func TestSnapshotCoversDefaultOnly(t *testing.T) {
	route := testutil.Memory(t)
	c := mustNew(t, testutil.Memory(t), composite.Route{Prefix: "/memories", Backend: route})
	if _, ok := backend.AsExecutor(c); ok {
		t.Fatal("memory default must not yield Executor")
	}
	snap, ok := backend.AsSnapshotter(c)
	if !ok {
		t.Fatal("memory default must yield Snapshotter")
	}
	put(t, c, "/scratch.txt", "before")
	put(t, c, "/memories/m.md", "kept")
	data, err := snap.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, "/scratch.txt", "after")
	put(t, c, "/extra.txt", "new")
	put(t, c, "/memories/m.md", "changed")
	if err := snap.Restore(t.Context(), data); err != nil {
		t.Fatal(err)
	}
	if readAll(t, c, "/scratch.txt") != "before" || has(t, c, "/extra.txt") {
		t.Fatal("def files not restored")
	}
	if got := readAll(t, c, "/memories/m.md"); got != "changed" {
		t.Fatalf("route file = %q, want untouched \"changed\"", got)
	}
}

func TestMemoriesPersistAcrossThreads(t *testing.T) {
	shared, err := kv.New(testutil.MemoryStore(t), "memories")
	if err != nil {
		t.Fatal(err)
	}
	thread := func() backend.Backend {
		return mustNew(t, testutil.Memory(t), composite.Route{Prefix: memmw.DefaultDir, Backend: shared})
	}
	a, b := thread(), thread()
	put(t, a, memmw.DefaultDir+"/color.md", "blue")
	put(t, a, "/scratch.txt", "mine")
	if got := readAll(t, b, memmw.DefaultDir+"/color.md"); got != "blue" {
		t.Fatalf("thread B memory = %q", got)
	}
	if has(t, b, "/scratch.txt") {
		t.Fatal("scratch file leaked across threads")
	}
}

// unwrapping hides every optional capability of the backend it wraps but
// exposes it through Unwrap.
type unwrapping struct{ backend.Backend }

func (u unwrapping) Unwrap() backend.Backend { return u.Backend }

func TestCapabilitiesThroughUnwrap(t *testing.T) {
	ctx := t.Context()
	c := mustNew(t, unwrapping{testutil.Memory(t)}, composite.Route{Prefix: "/m", Backend: unwrapping{testutil.Memory(t)}})
	if _, ok := c.(backend.Snapshotter); !ok {
		t.Fatal("default wrapping a Snapshotter must yield Snapshotter")
	}
	put(t, c, "/m/f", "x")
	if _, err := c.(backend.MultiEditor).MultiEdit(ctx, "/m/f", []backend.EditOp{{Old: "x", New: "y"}}); err != nil {
		t.Fatalf("MultiEdit through a route's Unwrap = %v", err)
	}
	ex, ok := mustNew(t, unwrapping{execOnly{testutil.Memory(t)}}).(backend.Executor)
	if !ok {
		t.Fatal("default wrapping an Executor must yield Executor")
	}
	if _, err := ex.Execute(ctx, "true", backend.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
}
