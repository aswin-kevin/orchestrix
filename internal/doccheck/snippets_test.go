package doccheck

import (
	"path/filepath"
	"testing"

	"github.com/aswin-kevin/orchestrix/internal/snippets"
)

// TestSnippetsCompile compiles the Go blocks of README.md, llms.txt and
// llms-full.txt that need only the core module (and orxdocker); orxotel and
// orxpg compile theirs in their own tests.
func TestSnippetsCompile(t *testing.T) {
	snippets.Check(t, filepath.Join("..", ".."), snippets.Core)
}
