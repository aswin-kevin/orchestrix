//go:build unix

package disk_test

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDiskRejectsNonRegularFiles(t *testing.T) {
	b, root := newDisk(t)
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
