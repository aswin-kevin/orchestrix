package tool

import (
	"errors"
	"fmt"
	"strings"
)

// MatchName reports whether name matches pattern. * matches any run of
// characters (including none and including "/"), ? matches exactly one
// character, every other character matches itself. Matching is
// case-sensitive; nothing is cleaned.
func MatchName(pattern, name string) bool {
	p, n := []rune(pattern), []rune(name)
	pi, ni, star, mark := 0, 0, -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ni
			pi++
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]):
			pi++
			ni++
		case star >= 0:
			mark++
			pi, ni = star+1, mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// ValidPattern reports why pattern cannot be used: empty, blank, or holding
// a NUL byte.
func ValidPattern(pattern string) error {
	switch {
	case pattern == "":
		return errors.New("tool: empty pattern")
	case strings.TrimSpace(pattern) == "":
		return errors.New("tool: blank pattern")
	case strings.ContainsRune(pattern, 0):
		return fmt.Errorf("tool: pattern %q holds a NUL byte", pattern)
	}
	return nil
}
