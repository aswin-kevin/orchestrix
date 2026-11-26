package tool

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8Boundary(t *testing.T) {
	s := "ab日本語cd" // bytes: a b then three 3-byte runes, then c d
	for max := 1; max < len(s); max++ {
		got := Truncate(s, max)
		if !utf8.ValidString(got) {
			t.Errorf("max=%d: invalid UTF-8 %q", max, got)
		}
	}
	// Cutting at byte 3 lands inside the first multibyte rune, so only "ab" is
	// kept.
	if got, want := Truncate(s, 3), "ab\n…[truncated 5 chars]"; got != want {
		t.Errorf("Truncate(s, 3) = %q, want %q", got, want)
	}
}

func TestTruncateUnchangedCases(t *testing.T) {
	for _, tt := range []struct {
		s   string
		max int
	}{{"hello", 5}, {"hello", 10}, {"hello", 0}, {"hello", -1}, {"", 3}} {
		if got := Truncate(tt.s, tt.max); got != tt.s {
			t.Errorf("Truncate(%q, %d) = %q, want unchanged", tt.s, tt.max, got)
		}
	}
}

func TestTruncateMarker(t *testing.T) {
	got := Truncate(strings.Repeat("x", 100), 10)
	if want := strings.Repeat("x", 10) + "\n…[truncated 90 chars]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
