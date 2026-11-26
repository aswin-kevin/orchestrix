package agent

import (
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func TestNewRejectsBadLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"retries below none", Config{OutputRetries: -2}, "agent: Config.OutputRetries -2 is invalid"},
		{"negative escalation", Config{EscalateMaxTokens: -1}, "agent: Config.EscalateMaxTokens -1 is negative"},
		{"nudge verbs", Config{BudgetNudge: "%s %s"}, "agent: Config.BudgetNudge has bad format verbs"},
		{"invalid output verbs", Config{InvalidOutputPrompt: "%d %d"}, "agent: Config.InvalidOutputPrompt has bad format verbs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Provider = llmtest.New()
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNewAcceptsPromptsWithoutVerbs(t *testing.T) {
	for _, cfg := range []Config{
		{BudgetNudge: "keep going"},
		{InvalidOutputPrompt: "try again"},
		{BudgetNudge: "100%% sure, %d left"},
	} {
		cfg.Provider = llmtest.New()
		if _, err := New(cfg); err != nil {
			t.Errorf("New(%+v) = %v, want nil", cfg, err)
		}
	}
}
