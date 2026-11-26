package profiles

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
)

// mustFS builds the filesystem middleware or fails the test.
func mustFS(t *testing.T, cfg filesystem.Config) *filesystem.Middleware {
	t.Helper()
	m, err := filesystem.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// mustPlanning builds the planning middleware or fails the test.
func mustPlanning(t *testing.T, cfg planning.Config) *planning.Middleware {
	t.Helper()
	m, err := planning.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
