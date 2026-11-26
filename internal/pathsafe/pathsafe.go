package pathsafe

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// emptyPlaceholder stands in for an empty id, which is not a usable segment.
const emptyPlaceholder = "empty"

// hashChars is how many hex characters of the id's SHA-256 are appended when
// the id had to be changed.
const hashChars = 8

// maxSafeChars caps the sanitized part of a segment.
const maxSafeChars = 64

var unsafeChars = regexp.MustCompile(`[^a-z0-9_-]`)

// Segment returns id as a path segment of only [a-z0-9_-], at most
// maxSafeChars long before an optional hash suffix. An id that is already
// such a segment is returned unchanged. Otherwise the id is lowercased (so
// segments are safe on case-insensitive file systems), every other character
// becomes "_", the result is cut to maxSafeChars, and "-" plus the first 8 hex
// characters of the id's SHA-256 are appended, so ids that sanitize alike
// (a/b and a_b, A and a, long ids sharing a prefix) still get distinct
// segments. An empty id becomes a fixed placeholder plus that hash.
func Segment(id string) string {
	safe := unsafeChars.ReplaceAllString(strings.ToLower(id), "_")
	if id == "" {
		safe = emptyPlaceholder
	}
	if len(safe) > maxSafeChars {
		safe = safe[:maxSafeChars] // ASCII only, so any cut is a character boundary
	}
	if safe == id {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return safe + "-" + hex.EncodeToString(sum[:])[:hashChars]
}
