package deferred

import (
	"slices"

	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// unlockedKey holds the tool name patterns unlocked in this run, in the
// order they were unlocked. A pattern is a name pattern as in tool.MatchName:
// "*" matches any run of characters and "?" exactly one. It is private to
// the run and checkpointed with it; a forked State inherits it. Read it with
// Unlocked; change it with Unlock and Lock.
var unlockedKey = state.NewKey[[]string]("deferred.unlocked", state.Private[[]string]())

// searchedKey holds the tool names search_tools unlocked in this run, in the
// order found. It lets a middleware that takes back its own unlocks (see
// Lock) leave alone the ones the model found by searching.
var searchedKey = state.NewKey[[]string]("deferred.searched", state.Private[[]string]())

// Searched returns the tool names search_tools unlocked, without duplicates,
// in the order found. The set accumulates across every run of the thread
// (the key is checkpointed with State) and holds exact names, not patterns.
// A search grant is never relocked: Lock only removes patterns a middleware
// names. s must not be nil.
func Searched(s *state.State) []string {
	names, _ := searchedKey.Get(s)
	return slices.Clone(names)
}

// recordSearched adds names to the searched set, skipping those present.
func recordSearched(s *state.State, names []string) {
	searchedKey.Update(s, func(current []string) []string {
		out := slices.Clone(current)
		for _, n := range names {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
		return out
	})
}

// Unlock adds patterns to the run's unlock set, skipping those already in
// it. Unlocking a name shows a deferred tool's schema to the model and makes
// a gated tool callable (see Deferrer and Gater). s must not be nil.
func Unlock(s *state.State, patterns ...string) {
	unlockedKey.Update(s, func(current []string) []string {
		// current is the stored slice itself: never append to it in place.
		unlocked := slices.Clone(current)
		for _, p := range patterns {
			if !slices.Contains(unlocked, p) {
				unlocked = append(unlocked, p)
			}
		}
		return unlocked
	})
}

// Lock removes each exact pattern from the unlock set (no glob matching
// between patterns); patterns not present are ignored. It is how a middleware
// takes back an unlock it granted. It removes the pattern whoever granted it:
// a pattern the host or another middleware also unlocked is removed too. s
// must not be nil.
func Lock(s *state.State, patterns ...string) {
	unlockedKey.Update(s, func(current []string) []string {
		// current is the stored slice itself: never modify it in place.
		return slices.DeleteFunc(slices.Clone(current), func(p string) bool {
			return slices.Contains(patterns, p)
		})
	})
}

// Unlocked reports whether any pattern of the run's unlock set matches
// name. It is false for a nil State.
func Unlocked(s *state.State, name string) bool {
	if s == nil {
		return false
	}
	patterns, _ := unlockedKey.Get(s)
	return matchesAny(patterns, name)
}

// matchesAny reports whether any of patterns matches the tool name.
func matchesAny(patterns []string, name string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return tool.MatchName(p, name) })
}
