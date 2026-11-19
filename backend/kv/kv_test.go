package kv_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/kv"
)

var ctx = context.Background()

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
	s, err := kv.NewDirStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestKVConformance(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		var n atomic.Int32
		s := kv.NewMemoryStore()
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
	s := kv.NewMemoryStore()
	for name, c := range map[string]struct {
		store kv.Store
		ns    string
	}{
		"nil store": {nil, "a"},
		"empty":     {s, ""},
		"too long":  {s, strings.Repeat("n", kv.MaxNamespaceBytes+1)},
		"NUL":       {s, "a\x00b"},
	} {
		if _, err := kv.New(c.store, c.ns); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := kv.New(s, strings.Repeat("n", kv.MaxNamespaceBytes)); err != nil {
		t.Fatalf("max-length namespace: %v", err)
	}
}

func TestDirStorePersistsAcrossInstances(t *testing.T) {
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
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := kv.NewDirStore(t.TempDir(), kv.WithClock(func() time.Time { return stamp }))
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
	for name, s := range map[string]kv.Store{
		"memory": kv.NewMemoryStore(),
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

func TestDirStoreKeyEncoding(t *testing.T) {
	dir := t.TempDir()
	b := newBackend(t, newDirStore(t, dir), "ns")
	cases := map[string]string{
		"/a b/ü?.md": "/a b/ü?.md",
		"/x/../y":    "/y",
		"/-rf":       "/-rf",
		"/a\\b:c*d":  "/a\\b:c*d",
	}
	for in, cleaned := range cases {
		if err := b.Write(ctx, in, []byte("v:"+in)); err != nil {
			t.Fatalf("Write(%q): %v", in, err)
		}
		res, err := b.Read(ctx, cleaned, 0, 0)
		if err != nil || !strings.Contains(text(res), "v:"+in) {
			t.Fatalf("Read(%q) = %+v, %v", cleaned, res, err)
		}
	}
	nsDir := filepath.Join(dir, base64.RawURLEncoding.EncodeToString([]byte("ns")))
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(cases) {
		t.Fatalf("%d entries, want %d", len(entries), len(cases))
	}
	for _, e := range entries {
		n := e.Name()
		if strings.ContainsAny(n, `/\`) || strings.HasPrefix(n, ".") || n == ".." {
			t.Errorf("unsafe entry name %q", n)
		}
	}
}

func TestKeyTooLong(t *testing.T) {
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
	m := newBackend(t, kv.NewMemoryStore(), "ns")
	if err := m.Write(ctx, long, []byte("x")); err != nil {
		t.Fatalf("MemoryStore must accept long keys: %v", err)
	}
}

func TestDirStoreAtomicWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("concurrent rename-over is not atomic on Windows")
	}
	dir := t.TempDir()
	s := newDirStore(t, dir)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Put(ctx, "ns", "/k", []byte(strings.Repeat("x", i+1))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	nsDir := filepath.Join(dir, base64.RawURLEncoding.EncodeToString([]byte("ns")))
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want exactly the key file", entries)
	}
	item, err := s.Get(ctx, "ns", "/k")
	if err != nil || len(item.Value) < 1 || len(item.Value) > 50 {
		t.Fatalf("Get = %d bytes, %v", len(item.Value), err)
	}

	if err := os.WriteFile(filepath.Join(nsDir, ".tmp-x"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := newBackend(t, s, "ns")
	infos, err := b.Ls(ctx, "/")
	if err != nil || len(infos) != 1 || infos[0].Path != "/k" {
		t.Fatalf("Ls = %+v, %v", infos, err)
	}
	globbed, err := b.Glob(ctx, "**/*", "/")
	if err != nil || len(globbed) != 1 {
		t.Fatalf("Glob = %+v, %v", globbed, err)
	}
}

func TestDirStoreFailedWriteLeavesNoTemp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-over-file rename semantics differ on Windows")
	}
	dir := t.TempDir()
	s := newDirStore(t, dir)
	nsDir := filepath.Join(dir, base64.RawURLEncoding.EncodeToString([]byte("ns")))
	// A directory squatting on the key's file name makes the final rename fail.
	if err := os.MkdirAll(filepath.Join(nsDir, base64.RawURLEncoding.EncodeToString([]byte("/k")), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "ns", "/k", []byte("v")); err == nil {
		t.Fatal("Put over a directory should fail")
	}
	entries, _ := os.ReadDir(nsDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestDirStorePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := filepath.Join(t.TempDir(), "fresh")
	s := newDirStore(t, dir)
	if err := s.Put(ctx, "ns", "/k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	nsDir := filepath.Join(dir, base64.RawURLEncoding.EncodeToString([]byte("ns")))
	for p, want := range map[string]fs.FileMode{
		dir:   0o700,
		nsDir: 0o700,
		filepath.Join(nsDir, base64.RawURLEncoding.EncodeToString([]byte("/k"))): 0o600,
	} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", p, got, want)
		}
	}
}

func TestMemoryStoreCopies(t *testing.T) {
	s := kv.NewMemoryStore()
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
	for name, s := range map[string]kv.Store{
		"memory": kv.NewMemoryStore(),
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
