package agent_test

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
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
