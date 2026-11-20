package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestApprovalOffline(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp) // os.TempDir on Windows
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	if err := run(ctx, &buf); err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, buf.String())
	}
	for _, want := range []string{"approved", "Hello from an approved write!"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, buf.String())
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("run left %d entries in the temp directory, want it cleaned up", len(left))
	}
}
