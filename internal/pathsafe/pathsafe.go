// Package pathsafe turns arbitrary identifiers (tool call ids, thread ids)
// into single path segments that are safe to use in backend file names.
package pathsafe

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// emptyPlaceholder stands in for an empty id, which is not a usable segment.
const emptyPlaceholder = "empty"

// hashChars is how many hex characters of the id's SHA-256 are appended when
// the id had to be changed.
const hashChars = 8

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Segment returns id as a path segment of only [A-Za-z0-9_-]. An id that is
// already safe is returned unchanged. Otherwise every other character becomes
// "_" and "-" plus the first 8 hex characters of the id's SHA-256 are
// appended, so ids that sanitize alike (a/b and a_b) still get distinct
// segments. An empty id becomes a fixed placeholder plus that hash.
func Segment(id string) string {
	safe := unsafeChars.ReplaceAllString(id, "_")
	if id == "" {
		safe = emptyPlaceholder
	}
	if safe == id {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return safe + "-" + hex.EncodeToString(sum[:])[:hashChars]
}
