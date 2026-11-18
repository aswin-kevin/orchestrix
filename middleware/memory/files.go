package memory

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/backend"
)

const (
	commentOpen  = "<!--"
	commentClose = "-->"
)

// Tags that could end or open a <memory> element from inside a file's
// content, in any case and with whitespace or attributes.
var (
	closingTag = regexp.MustCompile(`(?i)<\s*/\s*memory\b[^>]*>`)
	openingTag = regexp.MustCompile(`(?i)<\s*memory\b`)
)

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
		content = closingTag.ReplaceAllString(content, "&lt;/memory>")
		content = openingTag.ReplaceAllString(content, "&lt;memory")
		if content == "" {
			continue
		}
		content, truncated := capBytes(content, maxBytes)
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

// capBytes cuts s to at most limit bytes without splitting a rune.
func capBytes(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit], true
}
