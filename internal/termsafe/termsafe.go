package termsafe

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// String makes model-written text safe to print on a terminal. It shows
// as Go-style escapes, instead of sending them raw:
//   - control characters other than newline and tab: C0, DEL and C1, which
//     includes ESC and so every ANSI escape sequence (\x1b, \r, \u009b);
//   - bytes that are not valid UTF-8, which a terminal may read as 8-bit C1
//     controls (\x9b);
//   - invisible format characters that can reorder, hide or join text:
//     bidi marks, embeddings, overrides and isolates, zero-width spaces and
//     joiners, the word joiner, the byte order mark and the line and
//     paragraph separators (\u202e).
//
// So the text cannot move the cursor, recolor, retitle the window, write the
// clipboard or overwrite or disguise what is shown around it.
func String(s string) string {
	start := firstUnsafe(s)
	if start < 0 {
		return s
	}
	var sb strings.Builder
	sb.WriteString(s[:start])
	for i := start; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&sb, `\x%02x`, s[i])
		case r == '\r':
			sb.WriteString(`\r`)
		case r < 0x80 && isUnsafeRune(r):
			fmt.Fprintf(&sb, `\x%02x`, r)
		case isUnsafeRune(r):
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			sb.WriteString(s[i : i+size])
		}
		i += size
	}
	return sb.String()
}

// firstUnsafe returns the byte offset of the first invalid UTF-8 byte or
// unsafe rune in s, or -1 when String has nothing to escape.
func firstUnsafe(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 || isUnsafeRune(r) {
			return i
		}
		i += size
	}
	return -1
}

// isUnsafeRune reports whether r is a character String escapes: any C0
// control but newline and tab, DEL, any C1 control, and the invisible format
// characters listed on String.
func isUnsafeRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r == '\u2028', r == '\u2029', // line and paragraph separators
		r == '\u061c', // arabic letter mark
		// zero-width space, non-joiner, joiner; LTR and RTL marks
		r >= '\u200b' && r <= '\u200f',
		r == '\u2060', // word joiner
		// byte order mark (zero-width no-break space)
		r == '\ufeff',
		r >= '\u202a' && r <= '\u202e', // bidi embeddings and overrides
		r >= '\u2066' && r <= '\u2069': // bidi isolates
		return true
	}
	return false
}
