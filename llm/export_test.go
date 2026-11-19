package llm

import "time"

// SetFallbackClock replaces the cool-down clock of a provider built by
// NewFallback, so tests can move time forward.
func SetFallbackClock(p Provider, now func() time.Time) {
	p.(*fallback).now = now
}

// FallbackEntries returns a copy of the entries of a provider built by
// NewFallback, after defaults were applied.
func FallbackEntries(p Provider) []FallbackEntry {
	return append([]FallbackEntry(nil), p.(*fallback).entries...)
}
