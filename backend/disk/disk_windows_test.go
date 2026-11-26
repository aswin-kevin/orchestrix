package disk_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/disk"
)

// A drive-relative path ("C:foo") depends on the process's current
// directory on that drive, so a non-virtual backend refuses it.
func TestDiskNonVirtualRefusesDriveRelativePath(t *testing.T) {
	ctx := t.Context()
	b, root := newDisk(t, disk.Config{DisableVirtual: true})
	rel := filepath.VolumeName(root) + "foo.txt"
	if err := b.Write(ctx, rel, []byte("x")); !errors.Is(err, backend.ErrInvalidPath) {
		t.Errorf("Write(%q) = %v, want ErrInvalidPath", rel, err)
	}
	if _, err := b.Read(ctx, rel, 0, 0); !errors.Is(err, backend.ErrInvalidPath) {
		t.Errorf("Read(%q) = %v, want ErrInvalidPath", rel, err)
	}
	abs := filepath.Join(root, "foo.txt")
	if err := b.Write(ctx, abs, []byte("x")); err != nil {
		t.Errorf("Write(%q) = %v, want nil", abs, err)
	}
}
