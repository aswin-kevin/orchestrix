// Package promptsafe holds the helpers that keep untrusted text, such as file
// content, from breaking the framing of the system prompt segment it lands in.
package promptsafe

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// NewTagNeutralizer returns a function that escapes the "<" of every tag in
// its argument that could open or end one of the named elements, so content
// cannot break out of its frame. Matching ignores case and allows whitespace
// inside the tag; the escaped tag name is lowercased. Names are matched as
// whole words and must be plain identifiers.
func NewTagNeutralizer(tagNames ...string) func(string) string {
	quoted := make([]string, len(tagNames))
	for i, n := range tagNames {
		quoted[i] = regexp.QuoteMeta(n)
	}
	re := regexp.MustCompile(`(?i)<(\s*/?\s*)(` + strings.Join(quoted, "|") + `)\b`)
	return func(s string) string {
		return re.ReplaceAllStringFunc(s, func(tag string) string { return "&lt;" + strings.ToLower(tag[1:]) })
	}
}

// CapBytes cuts s to at most limit bytes without splitting a rune and reports
// whether it cut anything.
func CapBytes(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit], true
}
