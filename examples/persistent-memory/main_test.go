package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/middleware/memory"
)

func TestPersistentMemoryOffline(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	if err := run(ctx, &buf); err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, buf.String())
	}
	for _, want := range []string{"[tuesday] " + memory.ToolRecall + ":", "takes their coffee black"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, buf.String())
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("run left %d entries in the temp directory, want it cleaned up", len(left))
	}
}

func TestPersistentMemoryKeepsStore(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	dir := t.TempDir()
	*storeDir = dir
	t.Cleanup(func() { *storeDir = "" })

	if err := run(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) == 0 {
		t.Error("the -store directory is empty after the run, want the recorded memory kept")
	}
}
