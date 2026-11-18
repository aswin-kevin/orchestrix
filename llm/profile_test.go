package llm

import (
	"fmt"
	"sync"
	"testing"
)

func TestBuiltinProfiles(t *testing.T) {
	tests := []struct {
		model string
		want  Profile
	}{
		{"claude-opus-x", Profile{ContextWindow: 200_000, MaxOutput: 64_000, Thinking: true, Images: true, PDFs: true, Caching: true}},
		{"gpt-4o-mini", Profile{ContextWindow: 128_000, MaxOutput: 16_384, Images: true}},
		{"gpt-5.1", Profile{ContextWindow: 400_000, MaxOutput: 128_000, Thinking: true, Images: true, PDFs: true}},
		{"o3-mini", Profile{ContextWindow: 200_000, MaxOutput: 100_000, Thinking: true, Images: true}},
		{"o4-mini", Profile{ContextWindow: 200_000, MaxOutput: 100_000, Thinking: true, Images: true}},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := LookupProfile(tt.model); got != tt.want {
				t.Errorf("LookupProfile(%q) = %+v, want %+v", tt.model, got, tt.want)
			}
		})
	}
}

func TestLookupLongestPrefix(t *testing.T) {
	RegisterProfile("claude-x", Profile{ContextWindow: 1_000, MaxOutput: 100})

	override := LookupProfile("claude-x-2")
	if override.ContextWindow != 1_000 || override.MaxOutput != 100 {
		t.Errorf("claude-x-2 matched %+v, want the claude-x override", override)
	}
	if got := LookupProfile("claude-opus"); got.ContextWindow != 200_000 {
		t.Errorf("claude-opus matched %+v, want the built-in claude- profile", got)
	}
}

func TestLookupUnknownDefault(t *testing.T) {
	if got := LookupProfile("mystery-model"); got != DefaultProfile {
		t.Errorf("LookupProfile(unknown) = %+v, want DefaultProfile %+v", got, DefaultProfile)
	}
	if DefaultProfile.ContextWindow != 128_000 || DefaultProfile.MaxOutput != 8_192 {
		t.Errorf("DefaultProfile = %+v, want 128000 context and 8192 output", DefaultProfile)
	}
}

func TestLookupIsCaseSensitive(t *testing.T) {
	if got := LookupProfile("Claude-3"); got != DefaultProfile {
		t.Errorf("LookupProfile(%q) = %+v, want DefaultProfile", "Claude-3", got)
	}
}

func TestRegisterProfileConcurrentWithLookup(t *testing.T) {
	const workers = 8
	const iterations = 200

	var wg sync.WaitGroup
	for w := range workers {
		prefix := fmt.Sprintf("race-%d-", w)
		model := prefix + "model"
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range iterations {
				RegisterProfile(prefix, Profile{ContextWindow: i})
			}
		}()
		go func() {
			defer wg.Done()
			for range iterations {
				_ = LookupProfile(model)
			}
		}()
	}
	wg.Wait()

	for w := range workers {
		model := fmt.Sprintf("race-%d-model", w)
		if got := LookupProfile(model).ContextWindow; got != iterations-1 {
			t.Errorf("LookupProfile(%q).ContextWindow = %d, want last registered value %d", model, got, iterations-1)
		}
	}
}
