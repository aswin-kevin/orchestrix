package llm

import "time"

// SetFallbackClock replaces the cool-down clock of a Fallback, so tests can move time forward.
func SetFallbackClock(f *Fallback, now func() time.Time) {
	f.now = now
}

// FallbackEntries returns a copy of the entries of a provider built by
// NewFallback, after defaults were applied.
func FallbackEntries(f *Fallback) []FallbackEntry {
	return append([]FallbackEntry(nil), f.entries...)
}
