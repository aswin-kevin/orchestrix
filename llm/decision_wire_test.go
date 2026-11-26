package llm_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
)

// Fixtures follow the published API references, not the adapters' own
// assumptions.

var docQuestions = []llm.Question{
	{Name: "unsafe", Type: llm.QuestionYesNo, Instructions: "Is it unsafe?"},
	{Name: "label", Type: llm.QuestionChoice, Instructions: "Which label?",
		Choices: []llm.Choice{{Value: "bug"}, {Value: "billing", Description: "Payments"}}},
	{Name: "risk", Type: llm.QuestionScore, Instructions: "How risky?",
		Choices: []llm.Choice{{Value: "low", Description: "Harmless"}, {Value: "high", Description: "Dangerous"}}},
}

var docInput = []llm.Block{llm.TextBlock("rm -rf /")}

// OpenAI: score probabilities carry "value" as an integer, and usage nests
// cache_write_tokens in input_tokens_details and reports output_tokens.
func TestOpenAIDecisionsDocumentedReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"gpt-6-luna","answers":[
          {"type":"predicate","name":"unsafe","probability":0.9},
          {"type":"choice","name":"label","choice":"bug","confidence":0.8,
           "probabilities":[{"value":"bug","probability":0.85},{"value":"billing","probability":0.15}]},
          {"type":"score","name":"risk","score":0.9,"confidence":0.7,
           "probabilities":[{"value":0,"label":"low","probability":0.1},{"value":1,"label":"high","probability":0.9}]}],
         "usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":50},
          "output_tokens":7,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":127}}`)
	}))
	defer srv.Close()
	d, err := llm.NewDecider(llm.Config{Format: llm.FormatOpenAIDecisions, Model: "gpt-6-luna", APIKey: "test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := d.Decide(t.Context(), llm.DecisionRequest{Input: docInput, Questions: docQuestions})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := resp.Answers["risk"].Probabilities["high"]; got != 0.9 {
		t.Errorf("risk probability for high = %v, want 0.9", got)
	}
	if want := (llm.Usage{Input: 120, Output: 7, CacheRead: 20, CacheWrite: 50}); resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

// System One: a score question's criteria is an array of strings, lowest
// first, and a choice option without a description maps to null.
func TestSystemOneDocumentedRequest(t *testing.T) {
	var sent struct {
		Questions map[string]struct {
			Criteria json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Errorf("request body: %v", err)
		}
		fmt.Fprint(w, `{"model":"nimble","answers":{
          "unsafe":{"type":"noul","noul":0.9},
          "label":{"type":"choice","choice":"bug","probabilities":{"bug":0.9,"billing":0.1},"confidence":0.8},
          "risk":{"type":"score","score":0.9,"legend":{"0":"Harmless","1":"Dangerous"},"probabilities":{"0":0.1,"1":0.9},"confidence":0.7}},
         "usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	defer srv.Close()
	d, err := llm.NewDecider(llm.Config{Format: llm.FormatSystemOne, Model: "nimble", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Decide(t.Context(), llm.DecisionRequest{Input: docInput, Questions: docQuestions}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	var levels []string
	if err := json.Unmarshal(sent.Questions["risk"].Criteria, &levels); err != nil {
		t.Errorf("score criteria = %s, want an array of strings: %v", sent.Questions["risk"].Criteria, err)
	}
	var options map[string]*string
	if err := json.Unmarshal(sent.Questions["label"].Criteria, &options); err != nil {
		t.Fatalf("choice criteria: %v", err)
	}
	if options["bug"] != nil {
		t.Errorf("choice option without a description = %q, want null", *options["bug"])
	}
}
