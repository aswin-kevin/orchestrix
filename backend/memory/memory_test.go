package memory_test

import (
	"context"
	"encoding/json"
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
	snap, err := m.Snapshot()
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
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Write(ctx, "/keep/a.txt", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(ctx, "/extra.txt", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(snap); err != nil {
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
	if err := m.Restore(json.RawMessage(`not json`)); err == nil {
		t.Fatal("want error for malformed JSON")
	}
	bad := json.RawMessage(`{"files":{"/a":{"content":"!!!","mod_time":"2026-01-02T03:04:05Z"}}}`)
	if err := m.Restore(bad); err == nil {
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
