package summarize

import "testing"

// mustNew builds the middleware or fails the test.
func mustNew(t *testing.T, cfg Config) *Middleware {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
