package mcp

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/middleware/deferred"
)

// mustDeferred builds the deferred middleware or fails the test.
func mustDeferred(t *testing.T, cfg deferred.Config) *deferred.Middleware {
	t.Helper()
	m, err := deferred.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
