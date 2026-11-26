package kv_test

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

func TestDirStoreKeyEncoding(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	b := newBackend(t, newDirStore(t, dir), "ns")
	tests := map[string]string{
		"/a b/ü?.md": "/a b/ü?.md",
		"/x/../y":    "/y",
		"/-rf":       "/-rf",
		"/a\\b:c*d":  "/a\\b:c*d",
	}
	for in, cleaned := range tests {
		if err := b.Write(ctx, in, []byte("v:"+in)); err != nil {
			t.Fatalf("Write(%q): %v", in, err)
		}
		res, err := b.Read(ctx, cleaned, 0, 0)
		if err != nil || !strings.Contains(text(res), "v:"+in) {
			t.Fatalf("Read(%q) = %+v, %v", cleaned, res, err)
		}
	}
	nsDir := filepath.Join(dir, nsDirName("ns"))
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(tests) {
		t.Fatalf("%d entries, want %d", len(entries), len(tests))
	}
	for _, e := range entries {
		n := e.Name()
		if n != strings.ToLower(n) || len(n) != 64 || strings.Trim(n, "0123456789abcdef") != "" {
			t.Errorf("unsafe entry name %q", n)
		}
	}
}

func TestDirStoreAtomicWrite(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newDirStore(t, dir)
	if err := s.Put(ctx, "ns", "/k", []byte("x")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.Put(ctx, "ns", "/k", []byte(strings.Repeat("x", i+1))); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			item, err := s.Get(ctx, "ns", "/k")
			if err != nil {
				t.Error(err)
				return
			}
			if len(item.Value) < 1 || len(item.Value) > 50 || strings.Trim(string(item.Value), "x") != "" {
				t.Errorf("partial value seen: %d bytes", len(item.Value))
			}
		}()
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Join(dir, nsDirName("ns")))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want exactly the key file", entries)
	}
	item, err := s.Get(ctx, "ns", "/k")
	if err != nil || len(item.Value) < 1 || strings.Trim(string(item.Value), "x") != "" {
		t.Fatalf("Get = %q, %v", item.Value, err)
	}
}

func TestDirStoreIgnoresStrayTempFiles(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newDirStore(t, dir)
	b := newBackend(t, s, "ns")
	if err := b.Write(ctx, "/k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, nsDirName("ns"), ".tmp-x")
	if err := os.WriteFile(stray, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	infos, err := b.Ls(ctx, "/")
	if err != nil || len(infos) != 1 || infos[0].Path != "/k" {
		t.Fatalf("Ls = %+v, %v", infos, err)
	}
	globbed, err := b.Glob(ctx, "**/*", "/")
	if err != nil || len(globbed) != 1 {
		t.Fatalf("Glob = %+v, %v", globbed, err)
	}
}

func TestNewDirStoreRemovesOldTempFiles(t *testing.T) {
	dir := t.TempDir()
	nsDir := filepath.Join(dir, nsDirName("ns"))
	if err := os.MkdirAll(nsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, fresh, keep := filepath.Join(nsDir, ".tmp-old"), filepath.Join(nsDir, ".tmp-fresh"), filepath.Join(nsDir, "other")
	for _, p := range []string{old, fresh, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ancient := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{old, keep} {
		if err := os.Chtimes(p, ancient, ancient); err != nil {
			t.Fatal(err)
		}
	}
	newDirStore(t, dir)
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old temp file survived: %v", err)
	}
	for _, p := range []string{fresh, keep} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}

func TestDirStoreCaseVariantsStayDistinct(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newDirStore(t, dir)
	for _, pair := range [][2]string{{"/aa.md", "/aG.md"}, {"/a_b.md", "/aEb.md"}} {
		b := newBackend(t, s, "ns-"+pair[0])
		for _, p := range pair {
			if err := b.Write(ctx, p, []byte("content of "+p)); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range pair {
			res, err := b.Read(ctx, p, 0, 0)
			if err != nil || text(res) != "content of "+p {
				t.Fatalf("Read(%s) = %q, %v", p, text(res), err)
			}
		}
		infos, err := b.Ls(ctx, "/")
		if err != nil || len(infos) != 2 {
			t.Fatalf("Ls = %+v, %v", infos, err)
		}
	}
	for _, pair := range [][2]string{{"ab_", "abE"}, {"aa", "aG"}} {
		a, b := newBackend(t, s, pair[0]), newBackend(t, s, pair[1])
		if err := a.Write(ctx, "/f", []byte("first")); err != nil {
			t.Fatal(err)
		}
		if err := b.Write(ctx, "/f", []byte("second")); err != nil {
			t.Fatal(err)
		}
		ra, _ := a.Read(ctx, "/f", 0, 0)
		rb, _ := b.Read(ctx, "/f", 0, 0)
		if text(ra) != "first" || text(rb) != "second" {
			t.Fatalf("namespaces %v collide: %q %q", pair, text(ra), text(rb))
		}
	}
}

func TestDirStoreHeaderIntegrity(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newDirStore(t, dir)
	nsDir := filepath.Join(dir, nsDirName("ns"))
	if err := s.Put(ctx, "ns", "/good", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"empty":      {},
		"bad varint": {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
		"short key":  {50, 'a', 'b'},
	} {
		if err := os.WriteFile(filepath.Join(nsDir, keyFile("/bad")), content, 0o600); err != nil {
			t.Fatal(err)
		}
		infos, err := s.List(ctx, "ns", "/")
		if err != nil || len(infos) != 1 || infos[0].Key != "/good" {
			t.Fatalf("%s: List = %+v, %v", name, infos, err)
		}
		_, err = s.Get(ctx, "ns", "/bad")
		if err == nil || errors.Is(err, backend.ErrNotFound) || !strings.Contains(err.Error(), "corrupt") {
			t.Fatalf("%s: Get err = %v, want a corruption error", name, err)
		}
	}
	// A file named for /x but storing key /y is ignored.
	forged := append([]byte{2}, "/y"...)
	if err := os.WriteFile(filepath.Join(nsDir, keyFile("/x")), append(forged, "v"...), 0o600); err != nil {
		t.Fatal(err)
	}
	infos, _ := s.List(ctx, "ns", "/")
	for _, info := range infos {
		if info.Key == "/y" || info.Key == "/x" {
			t.Errorf("forged file listed: %+v", info)
		}
	}
	if _, err := s.Get(ctx, "ns", "/x"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Get of mismatched file = %v, want ErrNotFound", err)
	}
	// Size excludes the header.
	infos, _ = s.List(ctx, "ns", "/good")
	if len(infos) != 1 || infos[0].Size != 2 {
		t.Fatalf("List size = %+v, want 2", infos)
	}
}

func nsDirName(ns string) string {
	return strings.ToLower(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(ns)))
}

func keyFile(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func TestDirStoreFailedWriteLeavesNoTemp(t *testing.T) {
	ctx := t.Context()
	if runtime.GOOS == "windows" {
		t.Skip("directory-over-file rename semantics differ on Windows")
	}
	dir := t.TempDir()
	s := newDirStore(t, dir)
	nsDir := filepath.Join(dir, nsDirName("ns"))
	// A directory squatting on the key's file name makes the final rename
	// fail.
	if err := os.MkdirAll(filepath.Join(nsDir, keyFile("/k"), "sub"), 0o700); err != nil {
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
	ctx := t.Context()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := filepath.Join(t.TempDir(), "fresh")
	s := newDirStore(t, dir)
	if err := s.Put(ctx, "ns", "/k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	nsDir := filepath.Join(dir, nsDirName("ns"))
	for p, want := range map[string]fs.FileMode{
		dir:                                 0o700,
		nsDir:                               0o700,
		filepath.Join(nsDir, keyFile("/k")): 0o600,
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
