package agent

import (
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func TestNewRejectsBadPrompts(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"nudge verbs", Config{BudgetNudge: "%s %s"}, "agent: Config.BudgetNudge has bad format verbs"},
		{"invalid output verbs", Config{InvalidOutputPrompt: "%d %d"}, "agent: Config.InvalidOutputPrompt has bad format verbs"},
		{"invalid output int verb", Config{InvalidOutputPrompt: "bad: %d"}, "agent: Config.InvalidOutputPrompt has bad format verbs"},
		{"invalid output unknown verb", Config{InvalidOutputPrompt: "bad: %z"}, "agent: Config.InvalidOutputPrompt has bad format verbs"},
		{"nudge unknown verb", Config{BudgetNudge: "left: %z"}, "agent: Config.BudgetNudge has bad format verbs"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.Provider = llmtest.New()
			_, err := New(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNewAcceptsPromptVerbs(t *testing.T) {
	for _, cfg := range []Config{
		{BudgetNudge: "keep going"},
		{InvalidOutputPrompt: "try again"},
		{BudgetNudge: "100%% sure, %d left"},
		{InvalidOutputPrompt: "fix: %s"},
		{InvalidOutputPrompt: "fix: %v"},
	} {
		cfg.Provider = llmtest.New()
		if _, err := New(cfg); err != nil {
			t.Errorf("New(%+v) = %v, want nil", cfg, err)
		}
	}
}

// TestConfigNegatives pins the rule for numeric fields: zero means the
// default, the field's named -1 constant is accepted, and any other negative
// value fails New with an error naming the field.
func TestConfigNegatives(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(*Config)
		want string // "" means New succeeds
	}{
		{"zero", func(*Config) {}, ""},
		{"MaxTurns Unlimited", func(c *Config) { c.MaxTurns = Unlimited }, ""},
		{"OutputRetries NoOutputRetries", func(c *Config) { c.OutputRetries = NoOutputRetries }, ""},
		{"MaxTurns", func(c *Config) { c.MaxTurns = -2 }, "agent: Config.MaxTurns is negative"},
		{"OutputRetries", func(c *Config) { c.OutputRetries = -2 }, "agent: Config.OutputRetries is negative"},
		{"CacheBoundary", func(c *Config) { c.CacheBoundary = -1 }, "agent: Config.CacheBoundary is negative"},
		{"LeaseTTL", func(c *Config) { c.LeaseTTL = -1 }, "agent: Config.LeaseTTL is negative"},
		{"MaxTokens", func(c *Config) { c.MaxTokens = -1 }, "agent: Config.MaxTokens is negative"},
		{"MaxDuration", func(c *Config) { c.MaxDuration = -1 }, "agent: Config.MaxDuration is negative"},
		{"MaxTotalTokens", func(c *Config) { c.MaxTotalTokens = -1 }, "agent: Config.MaxTotalTokens is negative"},
		{"ModelTimeout", func(c *Config) { c.ModelTimeout = -1 }, "agent: Config.ModelTimeout is negative"},
		{"MaxConcurrency", func(c *Config) { c.MaxConcurrency = -1 }, "agent: Config.MaxConcurrency is negative"},
		{"TokenBudget", func(c *Config) { c.TokenBudget = -1 }, "agent: Config.TokenBudget is negative"},
		{"EscalateMaxTokens", func(c *Config) { c.EscalateMaxTokens = -1 }, "agent: Config.EscalateMaxTokens is negative"},
		{"MaxToolOutputChars", func(c *Config) { c.MaxToolOutputChars = -1 }, "agent: Config.MaxToolOutputChars is negative"},
		{"Settlement.MaxTurns", func(c *Config) { c.Settlement = &Settlement{MaxTurns: -1} }, "agent: Config.Settlement.MaxTurns is negative"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Provider: llmtest.New()}
			tt.set(&cfg)
			a, err := New(cfg)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("New = %v, want nil", err)
				}
				if got := a.Config(); got.MaxConcurrency != DefaultMaxConcurrency || got.LeaseTTL != DefaultLeaseTTL ||
					got.MaxToolOutputChars != DefaultMaxToolOutputChars {
					t.Errorf("Config() = %+v, want the zero fields defaulted", got)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("New = %v, want %q", err, tt.want)
			}
		})
	}
}
