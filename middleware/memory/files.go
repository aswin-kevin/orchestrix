package memory

import (
	"context"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
)

const (
	commentOpen  = "<!--"
	commentClose = "-->"
)

// neutralizeTags escapes any tag in s that could open or end one of the
// prompt's own elements.
var neutralizeTags = promptsafe.NewTagNeutralizer("memory_index", "memory")

// loadIndex renders the store's index for the system prompt, neutralizing a
// literal frame tag and capping it at MaxIndexBytes. A store that fails or
// has no entries gives an empty index.
func (m *Middleware) loadIndex(ctx context.Context, b backend.Backend) string {
	entries, err := m.store.Index(ctx, b)
	if err != nil || len(entries) == 0 {
		return ""
	}
	index := strings.TrimSpace(renderIndex(entries, nil))
	index = neutralizeTags(index)
	index, _ = promptsafe.CapBytes(index, MaxIndexBytes)
	return strings.TrimSpace(index)
}

// loadFiles reads each path from b and returns the files that have content.
// Instruction files are optional: one that cannot be read for any reason, is
// binary, or is empty once cleaned or capped is skipped.
func loadFiles(ctx context.Context, b backend.Backend, paths []string, maxBytes int) []File {
	if b == nil {
		return nil
	}
	var files []File
	for _, p := range paths {
		res, err := b.Read(ctx, p, 0, 0)
		if err != nil || res.Binary {
			continue
		}
		content := strings.TrimSpace(stripComments(strings.Join(res.Lines, "\n")))
		if content == "" {
			continue
		}
		content = neutralizeTags(content)
		content, truncated := promptsafe.CapBytes(content, maxBytes)
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		files = append(files, File{Path: p, Content: content, Truncated: truncated})
	}
	return files
}

// stripComments removes HTML comments, which may span lines. An unterminated
// comment drops the rest of the text.
func stripComments(s string) string {
	var out strings.Builder
	for {
		before, rest, found := strings.Cut(s, commentOpen)
		out.WriteString(before)
		if !found {
			return out.String()
		}
		_, s, found = strings.Cut(rest, commentClose)
		if !found {
			return out.String()
		}
	}
}
