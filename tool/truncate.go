package tool

import (
	"fmt"
	"unicode/utf8"
)

// Truncate shortens s to at most max bytes (not characters), cutting at a
// rune boundary so no UTF-8 sequence is split, and appends a marker that says
// how many characters (runes) were dropped so the model knows the output is
// incomplete. The marker is not counted in max. A non-positive max disables
// truncation.
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	dropped := utf8.RuneCountInString(s[cut:])
	return fmt.Sprintf("%s\n…[truncated %d chars]", s[:cut], dropped)
}
