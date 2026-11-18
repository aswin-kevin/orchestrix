package tool

import (
	"fmt"
	"unicode/utf8"
)

// Truncate shortens s to at most max bytes without splitting a UTF-8 sequence,
// appending a marker that says how many characters were dropped so the model
// knows the output is incomplete. A non-positive max disables truncation.
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
