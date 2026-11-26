package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// NewGeneration returns a random token of 24 hex characters naming one
// incarnation of a thread: a thread created again after Delete gets a new
// one, so its versions never match those of the deleted thread.
func NewGeneration() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// FormatVersion returns the version of a thread's saves-th save in
// generation gen: gen, a colon and saves in decimal.
func FormatVersion(gen string, saves int64) Version {
	return Version(gen + ":" + strconv.FormatInt(saves, 10))
}

// ParseVersion splits a version made by FormatVersion into its generation
// and save count. ok is false when v has no colon, an empty generation, or
// a save count that is not an integer of at least 1.
func ParseVersion(v Version) (gen string, saves int64, ok bool) {
	g, n, found := strings.Cut(string(v), ":")
	if !found || g == "" {
		return "", 0, false
	}
	saves, err := strconv.ParseInt(n, 10, 64)
	if err != nil || saves < 1 {
		return "", 0, false
	}
	return g, saves, true
}

// Conflict returns the error a Save reports when the thread's current
// version is not the expected one. It wraps ErrConflict. An empty current
// means the thread has no checkpoint; an empty expected means the caller
// expected to create it.
func Conflict(threadID string, expected, current Version) error {
	switch {
	case current == "":
		return fmt.Errorf("%w: thread %q has no checkpoint, expected version %q", ErrConflict, threadID, expected)
	case expected == "":
		return fmt.Errorf("%w: thread %q already exists at version %q", ErrConflict, threadID, current)
	default:
		return fmt.Errorf("%w: thread %q is at version %q, expected %q", ErrConflict, threadID, current, expected)
	}
}
