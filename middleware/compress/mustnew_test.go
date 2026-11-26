package compress

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/middleware/summarize"
)

// mustNew builds the middleware or fails the test.
func mustNew(t *testing.T, cfg Config) *Middleware {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// mustSummarize builds the summarize middleware or fails the test.
func mustSummarize(t *testing.T, cfg summarize.Config) *summarize.Middleware {
	t.Helper()
	m, err := summarize.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
