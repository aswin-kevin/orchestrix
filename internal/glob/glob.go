package glob

import (
	"path"
	"strings"
)

const (
	separator  = "/"
	anyDepth   = "**"
	anyInSeg   = '*'
	oneCharSeg = '?'
)

// Match reports whether name matches pattern. Both are normalised with
// path.Clean first, so "/a/./b/../c" and "/a/c" are the same path and
// ".." cannot be used to slip out of (or into) a pattern.
//
// Pattern syntax:
//
//	"*"   any run of characters within one path segment (never '/')
//	"?"   exactly one character within a segment
//	"**"  as a whole segment, zero or more whole segments
//
// Every other character matches itself. "**" glued to other text in a
// segment (e.g. "b**") behaves like a single "*".
func Match(pattern, name string) bool {
	return matchSegments(
		strings.Split(path.Clean(pattern), separator),
		strings.Split(path.Clean(name), separator),
	)
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == anyDepth {
			return matchAnyDepth(pattern[1:], name)
		}
		if len(name) == 0 || !matchSegment(pattern[0], name[0]) {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// matchAnyDepth lets a "**" segment swallow zero or more leading segments of
// name, then requires the rest of the pattern to match what is left.
func matchAnyDepth(rest, name []string) bool {
	for skip := 0; skip <= len(name); skip++ {
		if matchSegments(rest, name[skip:]) {
			return true
		}
	}
	return false
}

// matchSegment matches one path segment against a segment pattern using
// '*' and '?'.
func matchSegment(pattern, segment string) bool {
	return matchRunes([]rune(pattern), []rune(segment))
}

func matchRunes(pattern, text []rune) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case anyInSeg:
			for skip := 0; skip <= len(text); skip++ {
				if matchRunes(pattern[1:], text[skip:]) {
					return true
				}
			}
			return false
		case oneCharSeg:
			if len(text) == 0 {
				return false
			}
		default:
			if len(text) == 0 || text[0] != pattern[0] {
				return false
			}
		}
		pattern, text = pattern[1:], text[1:]
	}
	return len(text) == 0
}
