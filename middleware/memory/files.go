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

// memoryTag matches the start of an opening or closing <memory> tag in any
// case, with whitespace allowed inside it. Escaping its "<" keeps the content
// from opening or ending an element.
var memoryTag = regexp.MustCompile(`(?i)<(\s*/?\s*)memory\b`)

// indexTag is memoryTag for the <memory_index> element.
var indexTag = regexp.MustCompile(`(?i)<(\s*/?\s*)memory_index\b`)

// loadIndex reads the store index for the system prompt, neutralizing a
// literal closing tag and capping it at DefaultMaxIndexBytes. A missing or
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
	index = indexTag.ReplaceAllString(index, "&lt;${1}memory_index")
	index, _ = capBytes(index, DefaultMaxIndexBytes)
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
		content = memoryTag.ReplaceAllString(content, "&lt;${1}memory")
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
