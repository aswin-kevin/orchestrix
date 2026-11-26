package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/memory"
)

func TestMemoryConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend { return memory.New() })
}

func TestMemoryImplementsSnapshotter(t *testing.T) {
	var _ backend.Snapshotter = memory.New()
}

func TestMemoryUsesInjectedClock(t *testing.T) {
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	m := memory.New(memory.WithClock(func() time.Time { return stamp }))
	if err := m.Write(context.Background(), "/f", []byte("x")); err != nil {
		t.Fatal(err)
	}
	res, err := m.Read(context.Background(), "/f", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ModTime.Equal(stamp) {
		t.Fatalf("ModTime = %v, want %v", res.ModTime, stamp)
	}
}

func TestMemorySnapshotShape(t *testing.T) {
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	m := memory.New(memory.WithClock(func() time.Time { return stamp }))
	if err := m.Write(context.Background(), "/a/b.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"files":{"/a/b.txt":{"content":"aGk=","mod_time":"2026-01-02T03:04:05.000000006Z"}}}`
	if string(snap) != want {
		t.Fatalf("snapshot = %s\nwant      %s", snap, want)
	}
}

func TestMemorySnapshotRestore(t *testing.T) {
	ctx := context.Background()
	m := memory.New()
	if err := m.Write(ctx, "/keep/a.txt", []byte("original")); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Write(ctx, "/keep/a.txt", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(ctx, "/extra.txt", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(ctx, snap); err != nil {
		t.Fatal(err)
	}

	res, err := m.Read(ctx, "/keep/a.txt", 0, 0)
	if err != nil || len(res.Lines) != 1 || res.Lines[0] != "original" {
		t.Fatalf("after restore: %+v, %v", res, err)
	}
	if _, err := m.Read(ctx, "/extra.txt", 0, 0); err == nil {
		t.Fatal("file written after the snapshot must be gone")
	}
}

func TestMemoryRestoreRejectsBadInput(t *testing.T) {
	m := memory.New()
	if err := m.Restore(context.Background(), json.RawMessage(`not json`)); err == nil {
		t.Fatal("want error for malformed JSON")
	}
	bad := json.RawMessage(`{"files":{"/a":{"content":"!!!","mod_time":"2026-01-02T03:04:05Z"}}}`)
	if err := m.Restore(context.Background(), bad); err == nil {
		t.Fatal("want error for malformed base64")
	}
}

func TestMemoryIsolatesCallerBuffers(t *testing.T) {
	ctx := context.Background()
	m := memory.New()
	buf := []byte("abc")
	if err := m.Write(ctx, "/f", buf); err != nil {
		t.Fatal(err)
	}
	buf[0] = 'X'
	res, err := m.Read(ctx, "/f", 0, 0)
	if err != nil || res.Lines[0] != "abc" {
		t.Fatalf("stored content aliased the caller's slice: %+v, %v", res, err)
	}
}

func TestMemoryRefusesFilesOverReadLimit(t *testing.T) {
	ctx := context.Background()
	m := memory.New()
	big := make([]byte, backend.MaxReadFileBytes+1)
	if err := m.Write(ctx, "/big", big); err != nil {
		t.Fatal(err)
	}
	_, readErr := m.Read(ctx, "/big", 0, 1)
	_, editErr := m.Edit(ctx, "/big", "a", "b", false)
	_, multiErr := m.MultiEdit(ctx, "/big", []backend.EditOp{{Old: "a", New: "b"}})
	for name, err := range map[string]error{"Read": readErr, "Edit": editErr, "MultiEdit": multiErr} {
		var tooLarge *backend.TooLargeError
		if !errors.Is(err, backend.ErrTooLarge) || !errors.As(err, &tooLarge) || tooLarge.Size != int64(len(big)) {
			t.Errorf("%s error = %v, want ErrTooLarge reporting %d bytes", name, err, len(big))
		}
	}
	if err := m.Write(ctx, "/limit", big[:backend.MaxReadFileBytes]); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Read(ctx, "/limit", 0, 1); err != nil {
		t.Errorf("file at exactly the limit: %v", err)
	}
}

func TestSnapshotHonoursCtx(t *testing.T) {
	m := memory.New()
	if err := m.Write(context.Background(), "/a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Snapshot(cancelled) = %v, want context.Canceled", err)
	}
	if err := m.Write(context.Background(), "/b", []byte("y")); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(ctx, snap); !errors.Is(err, context.Canceled) {
		t.Errorf("Restore(cancelled) = %v, want context.Canceled", err)
	}
	if _, err := m.Read(context.Background(), "/b", 0, 0); err != nil {
		t.Errorf("a refused Restore must leave the backend untouched: %v", err)
	}
}
