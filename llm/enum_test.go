package llm_test

import (
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func TestEnumValuesUnchanged(t *testing.T) {
	tests := []struct{ name, got, want string }{
		{"EventMessageStart", string(llm.EventMessageStart), "message_start"},
		{"EventTextDelta", string(llm.EventTextDelta), "text_delta"},
		{"EventThinkingDelta", string(llm.EventThinkingDelta), "thinking_delta"},
		{"EventSignature", string(llm.EventSignature), "signature"},
		{"EventToolStart", string(llm.EventToolStart), "tool_start"},
		{"EventToolInputDelta", string(llm.EventToolInputDelta), "tool_input_delta"},
		{"EventProviderData", string(llm.EventProviderData), "provider_data"},
		{"EventMessageDelta", string(llm.EventMessageDelta), "message_delta"},
		{"EventMessageStop", string(llm.EventMessageStop), "message_stop"},
		{"StopEndTurn", string(llm.StopEndTurn), "end_turn"},
		{"StopToolUse", string(llm.StopToolUse), "tool_use"},
		{"StopMaxTokens", string(llm.StopMaxTokens), "max_tokens"},
		{"StopSequence", string(llm.StopSequence), "stop_sequence"},
		{"StopRefusal", string(llm.StopRefusal), "refusal"},
		{"StopPauseTurn", string(llm.StopPauseTurn), "pause_turn"},
		{"StopContentFilter", string(llm.StopContentFilter), "content_filter"},
		{"ThinkingEnabled", string(llm.ThinkingEnabled), "enabled"},
		{"ThinkingAdaptive", string(llm.ThinkingAdaptive), "adaptive"},
		{"ThinkingDisabled", string(llm.ThinkingDisabled), "disabled"},
		{"QuestionYesNo", string(llm.QuestionYesNo), "yes_no"},
		{"QuestionChoice", string(llm.QuestionChoice), "choice"},
		{"QuestionScore", string(llm.QuestionScore), "score"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestNewFallbackReturnsConcrete(t *testing.T) {
	var f *llm.Fallback
	f, err := llm.NewFallback(llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: llmtest.New(llmtest.Turn{Text: "a"})},
		{Provider: llmtest.New(llmtest.Turn{Text: "a"})},
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Complete(t.Context(), llm.Request{Messages: []llm.Message{llm.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Message.Text(); got != "a" {
		t.Errorf("text = %q, want %q", got, "a")
	}
}

func TestDefaultProfileReturnsCopy(t *testing.T) {
	p := llm.DefaultProfile()
	p.MaxOutput = 1
	got := llm.DefaultProfile()
	if got.MaxOutput != 8192 || got.ContextWindow != 128_000 {
		t.Errorf("DefaultProfile() = %+v, want MaxOutput 8192 and ContextWindow 128000", got)
	}
}

func TestQuestionChoicesValidated(t *testing.T) {
	req := llm.DecisionRequest{
		Input: []llm.Block{{Type: llm.BlockText, Text: "x"}},
		Questions: []llm.Question{{
			Name: "q", Type: llm.QuestionChoice, Instructions: "x",
			Choices: []llm.Choice{{Value: "a"}},
		}},
	}
	err := req.Validate()
	if err == nil || !strings.Contains(err.Error(), "needs at least 2 choices") {
		t.Errorf("Validate() = %v, want error containing %q", err, "needs at least 2 choices")
	}
}
