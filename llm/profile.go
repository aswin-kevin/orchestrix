package llm

import (
	"strings"
	"sync"
)

// Profile describes what a model supports, so the agent loop can size its
// context and enable only the features the model actually has.
type Profile struct {
	// ContextWindow is the total token budget for input and output.
	ContextWindow int
	// MaxOutput is the largest reply the model can produce in one turn.
	MaxOutput int
	// Thinking reports support for extended reasoning.
	Thinking bool
	// Images reports support for image inputs.
	Images bool
	// PDFs reports support for PDF inputs.
	PDFs bool
	// Caching reports support for prompt caching.
	Caching bool
}

// Fallback limits for models that match no registered prefix. The capability
// flags are off because an unknown model cannot be assumed to support them.
const (
	defaultContextWindow = 128_000
	defaultMaxOutput     = 8_192
)

// DefaultProfile returns what LookupProfile returns for a model that matches
// no registered prefix. Each call returns a fresh value.
func DefaultProfile() Profile {
	return Profile{ContextWindow: defaultContextWindow, MaxOutput: defaultMaxOutput}
}

// profilesMu guards profiles. Registration and lookup can happen from any
// goroutine, so reads take the read lock.
var profilesMu sync.RWMutex

// profiles maps a model-name prefix to its profile. It starts with the
// built-in table and grows through RegisterProfile.
var profiles = map[string]Profile{
	"claude-": {ContextWindow: 200_000, MaxOutput: 64_000, Thinking: true, Images: true, PDFs: true, Caching: true},
	"gpt-4o":  {ContextWindow: 128_000, MaxOutput: 16_384, Images: true},
	"gpt-5":   {ContextWindow: 400_000, MaxOutput: 128_000, Thinking: true, Images: true, PDFs: true},
	"o3":      {ContextWindow: 200_000, MaxOutput: 100_000, Thinking: true, Images: true},
	"o4":      {ContextWindow: 200_000, MaxOutput: 100_000, Thinking: true, Images: true},
}

// RegisterProfile sets the profile for every model whose name starts with
// modelPrefix. It replaces any profile already registered under the same
// prefix, including a built-in one. It is safe for concurrent use.
func RegisterProfile(modelPrefix string, p Profile) {
	profilesMu.Lock()
	defer profilesMu.Unlock()
	profiles[modelPrefix] = p
}

// LookupProfile returns the profile registered under the longest prefix of
// model, or DefaultProfile() when none matches. Matching is case-sensitive.
func LookupProfile(model string) Profile {
	p, ok := lookupProfile(model)
	if !ok {
		return DefaultProfile()
	}
	return p
}

// lookupProfile returns the profile registered under the longest prefix of
// model and reports whether any prefix matched.
func lookupProfile(model string) (Profile, bool) {
	profilesMu.RLock()
	defer profilesMu.RUnlock()

	var best Profile
	bestPrefix := ""
	matched := false
	for prefix, p := range profiles {
		if !strings.HasPrefix(model, prefix) {
			continue
		}
		if !matched || len(prefix) > len(bestPrefix) {
			best, bestPrefix, matched = p, prefix, true
		}
	}
	return best, matched
}
