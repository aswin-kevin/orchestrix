package deferred

import (
	"slices"

	"github.com/aswin-kevin/orchestrix/internal/glob"
	"github.com/aswin-kevin/orchestrix/state"
)

// UnlockedKey holds the tool name patterns unlocked in this run, in the
// order they were unlocked. A pattern is a glob over tool names: "*" matches
// any run of characters and "?" exactly one. It is private to the run and
// checkpointed with it; a forked State inherits it.
var UnlockedKey = state.NewKey[[]string]("deferred.unlocked", state.Private[[]string]())

// Unlock adds patterns to the run's unlock set, skipping those already in
// it. Unlocking a name shows a deferred tool's schema to the model and makes
// a gated tool callable (see Deferrer and Gater).
func Unlock(s *state.State, patterns ...string) {
	UnlockedKey.Update(s, func(current []string) []string {
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

// Unlocked reports whether any pattern of the run's unlock set matches
// name. It is false for a nil State.
func Unlocked(s *state.State, name string) bool {
	if s == nil {
		return false
	}
	patterns, _ := UnlockedKey.Get(s)
	return matchesAny(patterns, name)
}

// matchesAny reports whether any of patterns matches the tool name.
func matchesAny(patterns []string, name string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return glob.Match(p, name) })
}
