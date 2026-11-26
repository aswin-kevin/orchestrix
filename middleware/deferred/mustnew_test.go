package deferred

import "testing"

// mustDeferred builds the deferred middleware or fails the test.
func mustDeferred(t *testing.T, cfg Config) *Middleware {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
