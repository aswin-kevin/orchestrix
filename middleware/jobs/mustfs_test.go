package jobs

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
)

// mustNew builds the middleware or fails the test.
func mustNew(t *testing.T, cfg Config) *Middleware {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// mustFS builds the filesystem middleware or fails the test.
func mustFS(t *testing.T, cfg filesystem.Config) *filesystem.Middleware {
	t.Helper()
	m, err := filesystem.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
