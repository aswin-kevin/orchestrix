package kv_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/kv"
)

// corruptKey stores a file for key /c.txt whose header is unreadable.
func corruptKey(t *testing.T, dir string) {
	t.Helper()
	nsDir := filepath.Join(dir, nsDirName("ns"))
	if err := os.MkdirAll(nsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nsDir, keyFile("/c.txt")), []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptFileCanBeOverwrittenAndDeleted(t *testing.T) {
	ctx := t.Context()
	t.Run("write", func(t *testing.T) {
		dir := t.TempDir()
		corruptKey(t, dir)
		b, err := kv.New(newDirStore(t, dir), "ns")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Read(ctx, "/c.txt", 0, 0); err == nil || !strings.Contains(err.Error(), "corrupt") {
			t.Fatalf("Read = %v, want the corruption reported", err)
		}
		if err := b.Write(ctx, "/c.txt", []byte("fixed")); err != nil {
			t.Fatalf("Write over a corrupt file: %v", err)
		}
		if r, err := b.Read(ctx, "/c.txt", 0, 0); err != nil || len(r.Lines) != 1 || r.Lines[0] != "fixed" {
			t.Fatalf("Read after Write = %+v, %v", r, err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		dir := t.TempDir()
		corruptKey(t, dir)
		b, err := kv.New(newDirStore(t, dir), "ns")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Delete(ctx, "/c.txt"); err != nil {
			t.Fatalf("Delete of a corrupt file: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, nsDirName("ns"), keyFile("/c.txt"))); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("corrupt file still there: %v", err)
		}
	})
}

func TestNonRegularKeyFileIsNotCorrupt(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	// A directory where /d.txt's key file should be.
	if err := os.MkdirAll(filepath.Join(dir, nsDirName("ns"), keyFile("/d.txt")), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := kv.New(newDirStore(t, dir), "ns")
	if err != nil {
		t.Fatal(err)
	}
	err = b.Write(ctx, "/d.txt", []byte("x"))
	if err == nil || strings.Contains(err.Error(), "corrupt") || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Write = %v, want a non-regular-file error, not a corruption", err)
	}
}

func TestStaleTempSweepIgnoresInjectedClock(t *testing.T) {
	dir := t.TempDir()
	nsDir := filepath.Join(dir, nsDirName("ns"))
	if err := os.MkdirAll(nsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(nsDir, ".tmp-inflight")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A clock far ahead (a test clock, or a skewed one) must not make another
	// writer's in-flight temp look stale.
	later := func() time.Time { return time.Now().Add(48 * time.Hour) }
	if _, err := kv.NewDirStore(dir, kv.DirStoreConfig{Clock: later}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a fresh temp file was swept: %v", err)
	}
}

func TestKeyTooLongNamedOnce(t *testing.T) {
	ctx := t.Context()
	b, err := kv.New(newDirStore(t, t.TempDir()), "ns")
	if err != nil {
		t.Fatal(err)
	}
	err = b.Write(ctx, "/"+strings.Repeat("k", kv.MaxKeyBytes), []byte("x"))
	if !errors.Is(err, kv.ErrKeyTooLong) || strings.Count(err.Error(), "kv:") != 1 {
		t.Fatalf("Write = %v, want ErrKeyTooLong naming the package once", err)
	}
	if errors.Is(err, backend.ErrNotFound) {
		t.Fatal("a long key is not a missing one")
	}
}
