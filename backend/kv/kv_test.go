package kv_test

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
)

func newBackend(t *testing.T, s kv.Store, ns string) *kv.Backend {
	t.Helper()
	b, err := kv.New(s, ns)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newDirStore(t *testing.T, dir string) *kv.DirStore {
	t.Helper()
	s, err := kv.NewDirStore(dir, kv.DirStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestKVConformance(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		var n atomic.Int32
		s := testutil.MemoryStore(t)
		backendtest.Run(t, func(t *testing.T) backend.Backend {
			return newBackend(t, s, fmt.Sprintf("ns%d", n.Add(1)))
		})
	})
	t.Run("Dir", func(t *testing.T) {
		var n atomic.Int32
		backendtest.Run(t, func(t *testing.T) backend.Backend {
			return newBackend(t, newDirStore(t, t.TempDir()), fmt.Sprintf("ns%d", n.Add(1)))
		})
	})
}

func TestNewValidates(t *testing.T) {
	s := testutil.MemoryStore(t)
	for name, tt := range map[string]struct {
		store kv.Store
		ns    string
	}{
		"nil store": {nil, "a"},
		"empty":     {s, ""},
		"too long":  {s, strings.Repeat("n", kv.MaxNamespaceBytes+1)},
		"NUL":       {s, "a\x00b"},
	} {
		if _, err := kv.New(tt.store, tt.ns); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := kv.New(s, strings.Repeat("n", kv.MaxNamespaceBytes)); err != nil {
		t.Fatalf("max-length namespace: %v", err)
	}
}

func TestDirStorePersistsAcrossInstances(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	b1 := newBackend(t, newDirStore(t, dir), "mem")
	if err := b1.Write(ctx, "/memories/a.md", []byte("remember")); err != nil {
		t.Fatal(err)
	}
	first, err := b1.Read(ctx, "/memories/a.md", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b2 := newBackend(t, newDirStore(t, dir), "mem")
	got, err := b2.Read(ctx, "/memories/a.md", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text(got) != text(first) || !strings.Contains(text(got), "remember") {
		t.Fatalf("content = %q, want %q", text(got), text(first))
	}
	if !got.ModTime.Truncate(time.Second).Equal(first.ModTime.Truncate(time.Second)) {
		t.Fatalf("ModTime = %v, want %v", got.ModTime, first.ModTime)
	}
}

func TestDirStoreUsesInjectedClock(t *testing.T) {
	ctx := t.Context()
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := kv.NewDirStore(t.TempDir(), kv.DirStoreConfig{Clock: func() time.Time { return stamp }})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "ns", "/k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	item, err := s.Get(ctx, "ns", "/k")
	if err != nil {
		t.Fatal(err)
	}
	if !item.ModTime.Equal(stamp) {
		t.Fatalf("ModTime = %v, want %v", item.ModTime, stamp)
	}
}

func TestNamespacesIsolated(t *testing.T) {
	ctx := t.Context()
	for name, s := range map[string]kv.Store{
		"memory": testutil.MemoryStore(t),
		"dir":    newDirStore(t, t.TempDir()),
	} {
		t.Run(name, func(t *testing.T) {
			a, b := newBackend(t, s, "a"), newBackend(t, s, "b")
			if err := a.Write(ctx, "/x.md", []byte("from a")); err != nil {
				t.Fatal(err)
			}
			if err := b.Write(ctx, "/x.md", []byte("from b")); err != nil {
				t.Fatal(err)
			}
			if err := b.Write(ctx, "/only-b.md", []byte("b")); err != nil {
				t.Fatal(err)
			}
			ra, _ := a.Read(ctx, "/x.md", 0, 0)
			rb, _ := b.Read(ctx, "/x.md", 0, 0)
			if !strings.Contains(text(ra), "from a") || !strings.Contains(text(rb), "from b") {
				t.Fatalf("contents mixed: %q / %q", text(ra), text(rb))
			}
			infos, err := a.Ls(ctx, "/")
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != 1 || infos[0].Path != "/x.md" {
				t.Fatalf("a sees %+v, want only /x.md", infos)
			}
		})
	}
}

func TestNamespaceTraversal(t *testing.T) {
	ctx := t.Context()
	parent := t.TempDir()
	dir := filepath.Join(parent, "store")
	s := newDirStore(t, dir)
	for _, ns := range []string{"../escape", "a/../../x", "/abs", "..", ".", `..\win`} {
		b, err := kv.New(s, ns)
		if err != nil {
			t.Fatalf("New(%q): %v", ns, err)
		}
		if err := b.Write(ctx, "/../../k.md", []byte("x")); err != nil {
			t.Fatalf("Write in %q: %v", ns, err)
		}
		if _, err := b.Read(ctx, "/k.md", 0, 0); err != nil {
			t.Fatalf("Read in %q: %v", ns, err)
		}
	}
	err := filepath.WalkDir(parent, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != parent && p != dir && !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Errorf("file outside the store dir: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKeyTooLong(t *testing.T) {
	ctx := t.Context()
	long := "/" + strings.Repeat("k", 200)
	d := newBackend(t, newDirStore(t, t.TempDir()), "ns")
	err := d.Write(ctx, long, []byte("x"))
	if !errors.Is(err, kv.ErrKeyTooLong) {
		t.Fatalf("Write err = %v, want ErrKeyTooLong", err)
	}
	if !strings.Contains(err.Error(), "kkkk") {
		t.Errorf("error should name the path: %v", err)
	}
	if err := d.Write(ctx, "/"+strings.Repeat("k", kv.MaxKeyBytes-1), []byte("x")); err != nil {
		t.Fatalf("key at the limit: %v", err)
	}
	s := newDirStore(t, t.TempDir())
	if err := s.Put(ctx, "ns", long, nil); !errors.Is(err, kv.ErrKeyTooLong) {
		t.Fatalf("Put err = %v", err)
	}
	if _, err := s.List(ctx, "ns", long); !errors.Is(err, kv.ErrKeyTooLong) {
		t.Fatalf("List err = %v", err)
	}
	m := newBackend(t, testutil.MemoryStore(t), "ns")
	if err := m.Write(ctx, long, []byte("x")); err != nil {
		t.Fatalf("MemoryStore must accept long keys: %v", err)
	}
}

func TestMissingLongPathIsNotFound(t *testing.T) {
	ctx := t.Context()
	long := "/" + strings.Repeat("d", 200) + "/f.md"
	dirOnly := "/" + strings.Repeat("d", 200)
	b := newBackend(t, newDirStore(t, t.TempDir()), "ns")
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
	for _, p := range []string{long, dirOnly} {
		_, err := b.Read(ctx, p, 0, 0)
		check("Read", err)
		_, err = b.Ls(ctx, p)
		check("Ls", err)
		check("Delete", b.Delete(ctx, p))
		_, err = b.Glob(ctx, "*", p)
		check("Glob", err)
		_, err = b.Grep(ctx, "x", backend.GrepOptions{Path: p})
		check("Grep", err)
		_, err = b.Edit(ctx, p, "a", "b", false)
		check("Edit", err)
	}
}

func TestErrorMessagesWrapOnce(t *testing.T) {
	ctx := t.Context()
	b := newBackend(t, newDirStore(t, t.TempDir()), "ns")
	long := "/" + strings.Repeat("k", 200)
	for name, err := range map[string]error{
		"write too long": b.Write(ctx, long, nil),
		"delete missing": b.Delete(ctx, "/nope"),
	} {
		msg := err.Error()
		if strings.Count(msg, "kv:") != 1 || strings.Count(msg, "/nope") > 1 || strings.Count(msg, long) > 1 {
			t.Errorf("%s: repeated context in %q", name, msg)
		}
	}
}

func TestMemoryStoreUsesInjectedClock(t *testing.T) {
	ctx := t.Context()
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := kv.NewMemoryStore(kv.MemoryStoreConfig{Clock: func() time.Time { return stamp }})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "ns", "/k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	item, err := s.Get(ctx, "ns", "/k")
	if err != nil || !item.ModTime.Equal(stamp) {
		t.Fatalf("Get = %+v, %v", item, err)
	}
	infos, _ := s.List(ctx, "ns", "/")
	if len(infos) != 1 || !infos[0].ModTime.Equal(stamp) {
		t.Fatalf("List = %+v", infos)
	}
}

func TestMemoryStoreCopies(t *testing.T) {
	ctx := t.Context()
	s := testutil.MemoryStore(t)
	in := []byte("original")
	if err := s.Put(ctx, "ns", "/k", in); err != nil {
		t.Fatal(err)
	}
	in[0] = 'X'
	item, err := s.Get(ctx, "ns", "/k")
	if err != nil || string(item.Value) != "original" {
		t.Fatalf("Put aliased its argument: %q, %v", item.Value, err)
	}
	item.Value[0] = 'Y'
	again, _ := s.Get(ctx, "ns", "/k")
	if string(again.Value) != "original" {
		t.Fatalf("Get aliased the stored value: %q", again.Value)
	}
}

func TestStoreGetMissingIsNotFound(t *testing.T) {
	ctx := t.Context()
	for name, s := range map[string]kv.Store{
		"memory": testutil.MemoryStore(t),
		"dir":    newDirStore(t, t.TempDir()),
	} {
		if _, err := s.Get(ctx, "ns", "/nope"); !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("%s: Get err = %v", name, err)
		}
		if err := s.Delete(ctx, "ns", "/nope"); err != nil {
			t.Errorf("%s: Delete of a missing key: %v", name, err)
		}
		if infos, err := s.List(ctx, "ns", "/"); err != nil || len(infos) != 0 {
			t.Errorf("%s: List = %v, %v", name, infos, err)
		}
	}
}

func text(r backend.ReadResult) string { return strings.Join(r.Lines, "\n") }
