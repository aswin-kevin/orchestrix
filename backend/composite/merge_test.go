package composite_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/composite"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	memmw "github.com/aswin-kevin/orchestrix/middleware/memory"
)

func TestLsMergesRealEntriesWithMounts(t *testing.T) {
	ctx := t.Context()
	def := testutil.Memory(t)
	put(t, def, "/.orchestrix/other.txt", "o")
	put(t, def, "/.orchestrix/memories/shadow.md", "s")
	put(t, def, "/a.txt", "a")
	c := mustNew(t, def, composite.Route{Prefix: memmw.DefaultDir, Backend: testutil.Memory(t)})
	got, _ := c.Ls(ctx, "/.orchestrix")
	wantPaths(t, "Ls(/.orchestrix)", got, "/.orchestrix/memories", "/.orchestrix/other.txt")
	got, _ = c.Ls(ctx, "/")
	wantPaths(t, "Ls(/)", got, "/.orchestrix", "/a.txt")
}

func TestShadowedDefaultFilesHidden(t *testing.T) {
	ctx := t.Context()
	def, route := testutil.Memory(t), testutil.Memory(t)
	put(t, def, "/memories/old.md", "old shadow")
	c := mustNew(t, def, composite.Route{Prefix: "/memories", Backend: route})

	infos, err := c.Ls(ctx, "/memories")
	if err != nil || len(infos) != 0 {
		t.Fatalf("Ls = %+v, %v; want empty", infos, err)
	}
	infos, err = c.Glob(ctx, "**/*.md", "/")
	if err != nil || len(infos) != 0 {
		t.Fatalf("Glob = %+v, %v; want empty", infos, err)
	}
	ms, err := c.Grep(ctx, "old", backend.GrepOptions{Path: "/"})
	if err != nil || len(ms) != 0 {
		t.Fatalf("Grep = %+v, %v; want empty", ms, err)
	}
	if has(t, c, "/memories/old.md") {
		t.Fatal("shadowed file readable through the composite")
	}
	// Shadowed files must not starve a capped grep of real results.
	for _, p := range []string{"/memories/s1", "/memories/s2", "/memories/s3"} {
		put(t, def, p, "needle")
	}
	put(t, def, "/z.txt", "needle")
	ms, err = c.Grep(ctx, "needle", backend.GrepOptions{MaxResults: 1})
	if err != nil || len(ms) != 1 || ms[0].Path != "/z.txt" {
		t.Fatalf("capped Grep = %+v, %v; want /z.txt", ms, err)
	}
}

func TestGlobAndGrepMerge(t *testing.T) {
	ctx := t.Context()
	def, route := testutil.Memory(t), testutil.Memory(t)
	c := mustNew(t, def, composite.Route{Prefix: "/memories", Backend: route})
	put(t, c, "/a.md", "alpha hit")
	time.Sleep(20 * time.Millisecond)
	put(t, c, "/memories/b.md", "beta hit")

	infos, err := c.Glob(ctx, "**/*.md", "/")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "Glob", infos, "/memories/b.md", "/a.md")
	infos, _ = c.Glob(ctx, "/memories/*.md", "/")
	wantPaths(t, "Glob abs", infos, "/memories/b.md")
	infos, _ = c.Glob(ctx, "/*.md", "/")
	wantPaths(t, "Glob root only", infos, "/a.md")

	ms, err := c.Grep(ctx, "hit", backend.GrepOptions{Mode: backend.GrepContent})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Path != "/a.md" || ms[1].Path != "/memories/b.md" || ms[1].Line != 1 || ms[1].Text != "beta hit" {
		t.Fatalf("Grep = %+v", ms)
	}
	ms, _ = c.Grep(ctx, "hit", backend.GrepOptions{Mode: backend.GrepContent, MaxResults: 1})
	if len(ms) != 1 || ms[0].Path != "/a.md" {
		t.Fatalf("capped Grep = %+v", ms)
	}
	ms, _ = c.Grep(ctx, "hit", backend.GrepOptions{Glob: "memories/*.md"})
	if len(ms) != 1 || ms[0].Path != "/memories/b.md" {
		t.Fatalf("Grep with anchored glob = %+v", ms)
	}
	ms, _ = c.Grep(ctx, "hit", backend.GrepOptions{Glob: "*.md", Mode: backend.GrepCount})
	if len(ms) != 2 || ms[0].Count != 1 {
		t.Fatalf("Grep count = %+v", ms)
	}
}

func TestBasesAtAndBelowMount(t *testing.T) {
	ctx := t.Context()
	def, route := testutil.Memory(t), testutil.Memory(t)
	c := mustNew(t, def, composite.Route{Prefix: "/.orchestrix/memories", Backend: route})
	put(t, c, "/.orchestrix/memories/sub/n.md", "note")
	put(t, c, "/.orchestrix/memories/top.md", "note")

	infos, err := c.Glob(ctx, "**/*.md", "/.orchestrix/memories")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("Glob at mount = %q", paths(infos))
	}
	infos, err = c.Glob(ctx, "*.md", "/.orchestrix/memories/sub")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "Glob below mount", infos, "/.orchestrix/memories/sub/n.md")
	// Base is a mount ancestor def does not have: def contributes nothing.
	infos, err = c.Glob(ctx, "**/*.md", "/.orchestrix")
	if err != nil || len(infos) != 2 {
		t.Fatalf("Glob at ancestor = %q, %v", paths(infos), err)
	}
	if _, err = c.Glob(ctx, "*", "/missing"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Glob(missing) = %v, want ErrNotFound", err)
	}
	ms, err := c.Grep(ctx, "note", backend.GrepOptions{Path: "/.orchestrix"})
	if err != nil || len(ms) != 2 {
		t.Fatalf("Grep at ancestor = %+v, %v", ms, err)
	}
	ms, err = c.Grep(ctx, "note", backend.GrepOptions{Path: "/.orchestrix/memories/sub"})
	if err != nil || len(ms) != 1 || ms[0].Path != "/.orchestrix/memories/sub/n.md" {
		t.Fatalf("Grep below mount = %+v, %v", ms, err)
	}
	if _, err = c.Grep(ctx, "x", backend.GrepOptions{Path: "/missing"}); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Grep(missing) = %v, want ErrNotFound", err)
	}
}

func TestGrepDoublestarGlobsReachMounts(t *testing.T) {
	ctx := t.Context()
	def := testutil.Memory(t)
	put(t, def, "/x/memories/b.md", "hit")
	c := mustNew(t, def, composite.Route{Prefix: "/.orchestrix/memories", Backend: testutil.Memory(t)})
	put(t, c, "/.orchestrix/memories/a.md", "hit")
	const a, b = "/.orchestrix/memories/a.md", "/x/memories/b.md"
	for glob, want := range map[string][]string{
		"**/memories/*.md":              {a, b},
		"/.orchestrix/**/memories/*.md": {a},
		"**/.orchestrix/**":             {a},
		"/.orchestrix/memories/*":       {a},
		"/.orchestrix/*/a.md":           {a},
	} {
		ms, err := c.Grep(ctx, "hit", backend.GrepOptions{Glob: glob})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range ms {
			got = append(got, m.Path)
		}
		if !slices.Equal(got, want) {
			t.Errorf("Grep glob %q = %q, want %q", glob, got, want)
		}
	}
}

func TestGlobDropsMountAncestorsFromDefault(t *testing.T) {
	ctx := t.Context()
	def := testutil.Memory(t)
	put(t, def, "/m", "a file shadowed by the mount ancestor")
	c := mustNew(t, def, composite.Route{Prefix: "/m/sub", Backend: testutil.Memory(t)})
	put(t, c, "/m/sub/f", "x")
	infos, err := c.Glob(ctx, "/m", "/")
	if err != nil || len(infos) != 0 {
		t.Fatalf("Glob(/m) = %q, %v; want none", paths(infos), err)
	}
}

func TestNestedMountAncestorsProtected(t *testing.T) {
	ctx := t.Context()
	a := testutil.Memory(t)
	c := mustNew(t, testutil.Memory(t), composite.Route{Prefix: "/m", Backend: a}, composite.Route{Prefix: "/m/a/b", Backend: testutil.Memory(t)})
	put(t, a, "/a/other.txt", "keep")
	if err := c.Delete(ctx, "/m/a"); !errors.Is(err, composite.ErrMountPoint) {
		t.Fatalf("Delete(/m/a) = %v, want ErrMountPoint", err)
	}
	if !has(t, a, "/a/other.txt") {
		t.Fatal("refused delete removed outer mount content")
	}
	if err := c.Write(ctx, "/m/a", []byte("x")); !errors.Is(err, backend.ErrIsDir) {
		t.Fatalf("Write(/m/a) = %v, want ErrIsDir", err)
	}
	if _, err := c.Read(ctx, "/m/a", 0, 0); !errors.Is(err, backend.ErrIsDir) {
		t.Fatalf("Read(/m/a) = %v, want ErrIsDir", err)
	}
	infos, err := c.Ls(ctx, "/m/a")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "Ls(/m/a)", infos, "/m/a/b", "/m/a/other.txt")
}

func TestAncestorsFilteredFromFileResults(t *testing.T) {
	ctx := t.Context()
	// A def file standing where a mount ancestor is.
	def := testutil.Memory(t)
	put(t, def, "/m", "hit")
	c := mustNew(t, def, composite.Route{Prefix: "/m/sub", Backend: testutil.Memory(t)})
	ms, err := c.Grep(ctx, "hit", backend.GrepOptions{})
	if err != nil || len(ms) != 0 {
		t.Fatalf("Grep = %+v, %v; want none", ms, err)
	}

	// An outer mount's file standing where a nested mount's ancestor is.
	a := testutil.Memory(t)
	put(t, a, "/a", "hit")
	c2 := mustNew(t, testutil.Memory(t), composite.Route{Prefix: "/m", Backend: a}, composite.Route{Prefix: "/m/a/b", Backend: testutil.Memory(t)})
	infos, err := c2.Glob(ctx, "**", "/")
	if err != nil || len(infos) != 0 {
		t.Fatalf("Glob = %q, %v; want none", paths(infos), err)
	}
	ms, err = c2.Grep(ctx, "hit", backend.GrepOptions{})
	if err != nil || len(ms) != 0 {
		t.Fatalf("Grep = %+v, %v; want none", ms, err)
	}
}
