package orxpg_test

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/internal/snippets"
)

// TestSnippetsCompile compiles the Go blocks of the repository's README.md,
// llms.txt and llms-full.txt that import orxpg; the core module's
// internal/doccheck test compiles the rest.
func TestSnippetsCompile(t *testing.T) {
	snippets.Check(t, "..", snippets.OrxPg)
}
