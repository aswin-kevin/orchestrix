package backendtest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
)

// RunSnapshotter executes the conformance cases every backend.Snapshotter
// must pass against backends built by newBackend; each case gets fresh ones.
// The backend must satisfy backend.AsSnapshotter, or the kit fails.
func RunSnapshotter(t *testing.T, newBackend func(t *testing.T) backend.Backend) {
	t.Helper()
	fresh := func(t *testing.T) (backend.Backend, backend.Snapshotter) {
		t.Helper()
		b := newBackend(t)
		sn, ok := backend.AsSnapshotter(b)
		if !ok {
			t.Fatalf("%T does not implement backend.Snapshotter (see backend.AsSnapshotter)", b)
		}
		return b, sn
	}
	write := func(t *testing.T, b backend.Backend, path, content string) {
		t.Helper()
		if err := b.Write(t.Context(), path, []byte(content)); err != nil {
			t.Fatalf("Write(%s): %v", path, err)
		}
	}
	lines := func(t *testing.T, b backend.Backend, path string) []string {
		t.Helper()
		res, err := b.Read(t.Context(), path, 0, 0)
		if err != nil {
			t.Fatalf("Read(%s): %v", path, err)
		}
		return res.Lines
	}
	t.Run("RoundTrip", func(t *testing.T) {
		src, sn := fresh(t)
		write(t, src, "/a.txt", "one\ntwo\n")
		write(t, src, "/d/b.txt", "three\n")
		snap, err := sn.Snapshot(t.Context())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		dst, dsn := fresh(t)
		if err := dsn.Restore(t.Context(), snap); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		for _, p := range []string{"/a.txt", "/d/b.txt"} {
			if got, want := lines(t, dst, p), lines(t, src, p); !slices.Equal(got, want) {
				t.Errorf("%s = %q after restore, want %q", p, got, want)
			}
		}
	})
	t.Run("RestoreReplaces", func(t *testing.T) {
		src, sn := fresh(t)
		write(t, src, "/a.txt", "a\n")
		snap, err := sn.Snapshot(t.Context())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		dst, dsn := fresh(t)
		write(t, dst, "/x", "x\n")
		if err := dsn.Restore(t.Context(), snap); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if _, err := dst.Read(t.Context(), "/x", 0, 0); !errors.Is(err, backend.ErrNotFound) {
			t.Fatalf("Read(/x) after restore: err = %v, want ErrNotFound", err)
		}
	})
	t.Run("Cancelled", func(t *testing.T) {
		b, sn := fresh(t)
		write(t, b, "/a.txt", "a\n")
		snap, err := sn.Snapshot(t.Context())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := sn.Snapshot(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Snapshot with cancelled ctx: err = %v, want context.Canceled", err)
		}
		if err := sn.Restore(ctx, snap); !errors.Is(err, context.Canceled) {
			t.Errorf("Restore with cancelled ctx: err = %v, want context.Canceled", err)
		}
	})
	t.Run("BadData", func(t *testing.T) {
		b, sn := fresh(t)
		write(t, b, "/keep.txt", "keep\n")
		if err := sn.Restore(t.Context(), []byte("{")); err == nil {
			t.Fatal("Restore of malformed data returned nil")
		}
		if got := lines(t, b, "/keep.txt"); !slices.Equal(got, []string{"keep"}) {
			t.Fatalf("/keep.txt = %q after failed restore, want it unchanged", got)
		}
	})
}
