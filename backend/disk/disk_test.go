package disk_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/disk"
)

func newDisk(t *testing.T, opts ...disk.Option) (*disk.Backend, string) {
	t.Helper()
	root := t.TempDir()
	b, err := disk.New(root, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return b, root
}

func TestDiskConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		b, _ := newDisk(t)
		return b
	})
}

func TestDiskConformanceWithoutRipgrep(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		b, _ := newDisk(t, disk.WithRipgrep(false))
		return b
	})
}

func TestDiskNewRequiresExistingDirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.New(filepath.Join(root, "missing")); err == nil {
		t.Error("missing root must fail")
	}
	if _, err := disk.New(file); err == nil {
		t.Error("file root must fail")
	}
}

func TestDiskRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	b, root := newDisk(t)
	// A sibling of root that traversal would reach if cleaning were broken.
	secret := filepath.Join(filepath.Dir(root), "secret-"+filepath.Base(root))
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	for _, p := range []string{"../../etc/passwd", "/../x", "/../" + filepath.Base(secret), "../" + filepath.Base(secret)} {
		if _, err := b.Read(ctx, p, 0, 0); !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("Read(%q) = %v, want ErrNotFound inside root", p, err)
		}
	}
	if err := b.Write(ctx, "/../../planted.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "planted.txt")); err != nil {
		t.Fatalf("traversal write must land inside root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "planted.txt")); err == nil {
		t.Fatal("traversal write escaped root")
	}
}

func TestDiskRejectsSymlinkEscape(t *testing.T) {
	ctx := context.Background()
	b, root := newDisk(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(root, "link"))
	mustSymlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "file-link"))
	mustSymlink(t, filepath.Join(root, "missing-target"), filepath.Join(root, "dangling"))
	if err := b.Write(ctx, "/inside.txt", []byte("needle\n")); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Read(ctx, "/link/secret.txt", 0, 0); !errors.Is(err, backend.ErrOutsideRoot) {
		t.Errorf("read through dir symlink = %v, want ErrOutsideRoot", err)
	}
	if _, err := b.Read(ctx, "/file-link", 0, 0); !errors.Is(err, backend.ErrOutsideRoot) {
		t.Errorf("read through file symlink = %v, want ErrOutsideRoot", err)
	}
	if err := b.Write(ctx, "/link/new.txt", []byte("x")); !errors.Is(err, backend.ErrOutsideRoot) {
		t.Errorf("write through symlink = %v, want ErrOutsideRoot", err)
	}
	if err := b.Write(ctx, "/link/deep/new.txt", []byte("x")); !errors.Is(err, backend.ErrOutsideRoot) {
		t.Errorf("write to new path below symlink = %v, want ErrOutsideRoot", err)
	}
	if err := b.Write(ctx, "/dangling/x.txt", []byte("x")); err == nil {
		t.Error("write below a dangling symlink must fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Error("write escaped root")
	}
	if _, err := b.Ls(ctx, "/link"); !errors.Is(err, backend.ErrOutsideRoot) {
		t.Errorf("ls of escaping symlink = %v, want ErrOutsideRoot", err)
	}

	// Walks silently skip escaping entries.
	for _, rg := range []bool{true, false} {
		walker, err := disk.New(root, disk.WithRipgrep(rg))
		if err != nil {
			t.Fatal(err)
		}
		ls, err := walker.Ls(ctx, "/")
		if err != nil {
			t.Fatal(err)
		}
		for _, fi := range ls {
			if fi.Path == "/link" || fi.Path == "/file-link" {
				t.Errorf("Ls listed escaping symlink %s", fi.Path)
			}
		}
		globbed, err := walker.Glob(ctx, "**/*", "/")
		if err != nil || len(globbed) != 1 || globbed[0].Path != "/inside.txt" {
			t.Errorf("Glob = %+v, %v; want only /inside.txt", globbed, err)
		}
		hits, err := walker.Grep(ctx, "needle", backend.GrepOptions{Mode: backend.GrepFiles})
		if err != nil || len(hits) != 1 || hits[0].Path != "/inside.txt" {
			t.Errorf("Grep (ripgrep=%v) = %+v, %v; want only /inside.txt", rg, hits, err)
		}
	}

	// Deleting a symlink removes the link, never its target.
	if err := b.Delete(ctx, "/file-link"); err != nil {
		t.Fatalf("delete symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatalf("symlink target was deleted: %v", err)
	}
}

func TestDiskSymlinkInsideRootIsAllowed(t *testing.T) {
	ctx := context.Background()
	b, root := newDisk(t)
	if err := b.Write(ctx, "/real/a.txt", []byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, filepath.Join(root, "real"), filepath.Join(root, "alias"))
	res, err := b.Read(ctx, "/alias/a.txt", 0, 0)
	if err != nil || len(res.Lines) != 1 {
		t.Fatalf("read via inside symlink: %+v, %v", res, err)
	}
}

func TestDiskDeleteRootFails(t *testing.T) {
	b, root := newDisk(t)
	if err := b.Delete(context.Background(), "/"); err == nil {
		t.Fatal("deleting root must fail")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("root vanished")
	}
}

func TestDiskWritePreservesModeAndLeavesNoTemp(t *testing.T) {
	ctx := context.Background()
	b, root := newDisk(t)
	target := filepath.Join(root, "script.sh")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Write(ctx, "/script.sh", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := b.Write(ctx, "/sub/fresh.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	assertMode(t, target, 0o755)
	assertMode(t, filepath.Join(root, "sub", "fresh.txt"), 0o644)
	assertMode(t, filepath.Join(root, "sub"), 0o755)

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("root has leftovers: %v", entries)
	}
}

func TestDiskNonVirtualMode(t *testing.T) {
	ctx := context.Background()
	b, root := newDisk(t, disk.WithVirtual(false))
	real := filepath.Join(root, "dir", "a.txt")
	if err := b.Write(ctx, real, []byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(real); err != nil {
		t.Fatalf("file not at the real path: %v", err)
	}
	ls, err := b.Ls(ctx, filepath.Join(root, "dir"))
	if err != nil || len(ls) != 1 || ls[0].Path != real {
		t.Fatalf("Ls = %+v, %v; want real path %s", ls, err, real)
	}
	if _, err := b.Read(ctx, "/etc/passwd", 0, 0); !errors.Is(err, backend.ErrOutsideRoot) {
		t.Fatalf("path outside root = %v, want ErrOutsideRoot", err)
	}
	if err := b.Delete(ctx, root); err == nil {
		t.Fatal("deleting root must fail in non-virtual mode too")
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func TestDiskRefusesFilesOverReadLimit(t *testing.T) {
	ctx := context.Background()
	b, root := newDisk(t)
	// A sparse file: large on paper, cheap to create.
	f, err := os.Create(filepath.Join(root, "big"))
	if err != nil {
		t.Fatal(err)
	}
	size := int64(backend.MaxReadFileBytes + 1)
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	_, readErr := b.Read(ctx, "/big", 0, 1)
	_, editErr := b.Edit(ctx, "/big", "a", "b", false)
	_, multiErr := b.MultiEdit(ctx, "/big", []backend.EditOp{{Old: "a", New: "b"}})
	for name, err := range map[string]error{"Read": readErr, "Edit": editErr, "MultiEdit": multiErr} {
		var tooLarge *backend.TooLargeError
		if !errors.Is(err, backend.ErrTooLarge) || !errors.As(err, &tooLarge) || tooLarge.Size != size {
			t.Errorf("%s error = %v, want ErrTooLarge reporting %d bytes", name, err, size)
		}
	}
}

func TestDiskRealPath(t *testing.T) {
	b, tempRoot := newDisk(t)
	root, err := filepath.EvalSymlinks(tempRoot) // RealPath returns symlink-free paths
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		"":             root,
		"/":            root,
		"/sub":         filepath.Join(root, "sub"),
		"/sub/new.txt": filepath.Join(root, "sub", "new.txt"),
		"/../sub":      filepath.Join(root, "sub"),
	} {
		got, err := b.RealPath(p)
		if err != nil || got != want {
			t.Errorf("RealPath(%q) = %q, %v; want %q", p, got, err, want)
		}
	}

	outside := t.TempDir()
	mustSymlink(t, outside, filepath.Join(root, "link"))
	if _, err := b.RealPath("/link"); !errors.Is(err, backend.ErrOutsideRoot) {
		t.Errorf("RealPath through outward symlink = %v, want ErrOutsideRoot", err)
	}
	if _, err := b.RealPath("/link/deep/x"); !errors.Is(err, backend.ErrOutsideRoot) {
		t.Errorf("RealPath below outward symlink = %v, want ErrOutsideRoot", err)
	}

	host, err := disk.New(root, disk.WithVirtual(false))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := host.RealPath(""); err != nil || got != root {
		t.Errorf("host RealPath(\"\") = %q, %v; want root %q", got, err, root)
	}
	if _, err := host.RealPath(filepath.Join(root, "..")); !errors.Is(err, backend.ErrOutsideRoot) {
		t.Errorf("host RealPath(root/..) = %v, want ErrOutsideRoot", err)
	}
}
