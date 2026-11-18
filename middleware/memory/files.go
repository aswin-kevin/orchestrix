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

// loadIndex reads the store index for the system prompt, neutralizing a
// literal frame tag and capping it at DefaultMaxIndexBytes. A missing or
// unreadable index is empty.
func loadIndex(ctx context.Context, b backend.Backend, p string) string {
	if b == nil {
		return ""
	}
	res, err := b.Read(ctx, p, 0, 0)
	if err != nil || res.Binary {
		return ""
	}
	index := strings.TrimSpace(strings.Join(res.Lines, "\n"))
	index = neutralizeTags(index)
	index, _ = promptsafe.CapBytes(index, DefaultMaxIndexBytes)
	return strings.TrimSpace(index)
}

// loadFiles reads each path from b and returns the files that have content.
// Instruction files are optional: one that cannot be read for any reason, is
// binary, or is empty once cleaned is skipped.
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
