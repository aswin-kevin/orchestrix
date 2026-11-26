package agent

import (
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func TestNewRejectsBadLimits(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"retries below none", Config{OutputRetries: -2}, "agent: Config.OutputRetries -2 is invalid"},
		{"negative escalation", Config{EscalateMaxTokens: -1}, "agent: Config.EscalateMaxTokens -1 is negative"},
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
