//go:build unix

package disk_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend/disk"
)

// TestRewriteKeepsMode checks that a rewrite of a private file never exposes
// its new content more widely: the temporary file must already carry the
// target's mode when it is created, before any byte is written to it.
func TestRewriteKeepsMode(t *testing.T) {
	b, root := newDisk(t, disk.Config{})
	target := filepath.Join(root, "key")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var temps int
	defer disk.SetTempCreatedHook(func(f *os.File) {
		temps++
		info, err := f.Stat()
		if err != nil {
			t.Errorf("stat temp: %v", err)
			return
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("temp created with mode %o, want 600", got)
		}
	})()
	ctx := context.Background()
	if err := b.Write(ctx, "/key", []byte("new secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Edit(ctx, "/key", "new", "newer", false); err != nil {
		t.Fatal(err)
	}
	if temps != 2 {
		t.Fatalf("hook saw %d temp files, want 2", temps)
	}
	assertMode(t, target, 0o600)
}

func TestDiskRejectsNonRegularFiles(t *testing.T) {
	b, root := newDisk(t, disk.Config{})
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	ctx := context.Background()
	if _, err := b.Read(ctx, "/pipe", 0, 0); err == nil {
		t.Error("Read of a FIFO must fail instead of blocking")
	}
	if _, err := b.Edit(ctx, "/pipe", "a", "b", false); err == nil {
		t.Error("Edit of a FIFO must fail")
	}
	if err := b.Write(ctx, "/pipe", []byte("x")); err == nil {
		t.Error("Write over a FIFO must fail")
	}
}
