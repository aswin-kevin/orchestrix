package deferred

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
)

// mustDeferred builds the deferred middleware or fails the test.
func mustDeferred(t *testing.T, cfg Config) *Middleware {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
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
