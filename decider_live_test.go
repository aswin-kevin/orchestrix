package orchestrix_test

import (
	"context"
	"os"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
)

// The live test reads its own variables, so it never disturbs the text-model
// live tests: the full spec, the endpoint and the key.
func TestLiveDecide(t *testing.T) {
	spec := os.Getenv("ORX_TEST_DECIDER_MODEL")
	baseURL := os.Getenv("ORX_TEST_DECIDER_BASE_URL")
	key := os.Getenv("ORX_TEST_DECIDER_API_KEY")
	if spec == "" || baseURL == "" || key == "" {
		t.Skip("ORX_TEST_DECIDER_MODEL, ORX_TEST_DECIDER_BASE_URL and ORX_TEST_DECIDER_API_KEY not set; skipping live test")
	}
	cfg, err := llm.ParseModel(spec)
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseURL, cfg.APIKey = baseURL, key
	d, err := llm.NewDecider(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	resp, err := d.Decide(ctx, llm.DecisionRequest{
		Input: []llm.Block{llm.TextBlock("My card was charged twice for the same order. Please refund the duplicate payment today, it is blocking my rent.")},
		Questions: []llm.Question{
			{Name: "refund", Type: llm.QuestionYesNo, Instructions: "Is the customer asking for a refund?"},
			{Name: "team", Type: llm.QuestionChoice, Instructions: "Which team should handle this?", Choices: []llm.Choice{
				{Value: "payments", Description: "billing, charges and refunds"},
				{Value: "mobile", Description: "app bugs and crashes"},
				{Value: "identity", Description: "login and account access"},
			}},
			{Name: "urgency", Type: llm.QuestionScore, Instructions: "How urgent is the request?", Choices: []llm.Choice{
				{Value: "low"}, {Value: "medium"}, {Value: "high"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Answers["refund"].Yes; got <= 0.5 {
		t.Errorf("refund yes = %v, want > 0.5", got)
	}
	if got := resp.Answers["team"].Choice; got != "payments" {
		t.Errorf("team = %q, want payments", got)
	}
	if got := len(resp.Answers["urgency"].Probabilities); got != 3 {
		t.Errorf("urgency probabilities = %d, want 3", got)
	}
}
