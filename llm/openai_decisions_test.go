package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const oaReplyJSON = `{"model":"decider-1","answers":[
 {"type":"predicate","name":"refund","probability":0.9},
 {"type":"choice","name":"team","choice":"payments","confidence":0.8,"probabilities":[{"value":"payments","probability":0.8},{"value":"mobile","probability":0.2}]},
 {"type":"score","name":"urgency","score":0.7,"confidence":0.6,"probabilities":[{"value":0,"label":"low","probability":0.3},{"value":1,"label":"high","probability":0.7}]}],
 "usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":15},
  "output_tokens":4,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":124}}`

func newTestOpenAIDecisions(t *testing.T, baseURL string) Decider {
	t.Helper()
	d, err := NewDecider(Config{Format: FormatOpenAIDecisions, BaseURL: baseURL, APIKey: "key-1", Model: "decider-1",
		MaxRetries: NoRetries, RetryInterval: time.Millisecond, Headers: map[string]string{"X-Extra": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func oaServer(t *testing.T, reply string, gotBody *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions" || r.Method != http.MethodPost {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer key-1" || r.Header.Get("X-Extra") != "yes" ||
			r.Header.Get("content-type") != "application/json" {
			t.Errorf("headers: %v", r.Header)
		}
		if gotBody != nil {
			*gotBody, _ = io.ReadAll(r.Body)
		}
		io.WriteString(w, reply)
	}))
}

const oaWireQuestions = `[
 {"type":"predicate","name":"refund","instructions":"Is the customer asking for a refund?"},
 {"type":"choice","name":"team","instructions":"Which team?","choices":[{"value":"payments","description":"billing, refunds"},{"value":"mobile","description":"app bugs"}]},
 {"type":"score","name":"urgency","instructions":"How urgent?","levels":[{"label":"low","description":"cosmetic"},{"label":"high","description":"outage"}]}]`

func TestOpenAIDecisionsRequest(t *testing.T) {
	var body []byte
	srv := oaServer(t, oaReplyJSON, &body)
	defer srv.Close()
	d := newTestOpenAIDecisions(t, srv.URL+"/v1")

	req := soRequest()
	req.Input = []Block{TextBlock("first"), TextBlock("second")}
	if _, err := d.Decide(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	assertJSONEq(t, body, `{"model":"decider-1","input":"first\n\nsecond","questions":`+oaWireQuestions+`}`)

	req.Model = "other"
	req.Input = []Block{
		TextBlock("look"),
		{Type: BlockImage, Source: &Source{Type: "base64", MediaType: "image/png", Data: "QUJD"}},
		{Type: BlockImage, Source: &Source{Type: "url", URL: "https://example.com/a.png"}},
	}
	if _, err := d.Decide(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	assertJSONEq(t, body, `{"model":"other","input":[{"role":"user","content":[
	 {"type":"input_text","text":"look"},
	 {"type":"input_image","image_url":"data:image/png;base64,QUJD"},
	 {"type":"input_image","image_url":"https://example.com/a.png"}]}],"questions":`+oaWireQuestions+`}`)
}

func TestOpenAIDecisionsAnswers(t *testing.T) {
	srv := oaServer(t, oaReplyJSON, nil)
	defer srv.Close()
	resp, err := newTestOpenAIDecisions(t, srv.URL+"/v1/").Decide(t.Context(), soRequest())
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.Answers["refund"]; a.Type != QuestionYesNo || a.Yes != 0.9 {
		t.Errorf("refund = %+v", a)
	}
	if a := resp.Answers["team"]; a.Type != QuestionChoice || a.Choice != "payments" || a.Confidence != 0.8 ||
		a.Probabilities["payments"] != 0.8 || a.Probabilities["mobile"] != 0.2 {
		t.Errorf("team = %+v", a)
	}
	if a := resp.Answers["urgency"]; a.Type != QuestionScore || a.Score != 0.7 || a.Confidence != 0.6 ||
		a.Probabilities["low"] != 0.3 || a.Probabilities["high"] != 0.7 || len(a.Probabilities) != 2 {
		t.Errorf("urgency = %+v", a)
	}
	if resp.Model != "decider-1" || resp.Usage != (Usage{Input: 120, Output: 4, CacheRead: 40, CacheWrite: 15}) {
		t.Errorf("model/usage = %q %+v", resp.Model, resp.Usage)
	}
}

func TestOpenAIDecisionsRefusal(t *testing.T) {
	srv := oaServer(t, `{"answers":[{"type":"refusal","name":"refund"},
	 {"type":"refusal","name":"team"},
	 {"type":"score","name":"urgency","score":0,"probabilities":[]}]}`, nil)
	defer srv.Close()
	resp, err := newTestOpenAIDecisions(t, srv.URL+"/v1").Decide(t.Context(), soRequest())
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.Answers["refund"]; !a.Refused || a.Type != QuestionYesNo {
		t.Errorf("refund = %+v", a)
	}
	if a := resp.Answers["team"]; !a.Refused || a.Type != QuestionChoice {
		t.Errorf("team = %+v", a)
	}
}

func TestOpenAIDecisionsMissingAnswer(t *testing.T) {
	srv := oaServer(t, `{"answers":[{"type":"predicate","name":"refund","probability":0.5},
	 {"type":"predicate","name":"stray","probability":0.5}]}`, nil)
	defer srv.Close()
	_, err := newTestOpenAIDecisions(t, srv.URL+"/v1").Decide(t.Context(), soRequest())
	if err == nil || !strings.Contains(err.Error(), `no answer for question "team"`) {
		t.Errorf("err = %v", err)
	}
}

func TestOpenAIDecisionsRejectsDocument(t *testing.T) {
	d := newTestOpenAIDecisions(t, "http://unused.invalid/v1")
	req := soRequest()
	req.Input = []Block{TextBlock("ok"), {Type: BlockDocument}}
	_, err := d.Decide(t.Context(), req)
	if err == nil || err.Error() != "llm: openai-decisions does not accept document input" {
		t.Errorf("err = %v", err)
	}
}

func TestParseModelOpenAIDecisions(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("OPENAI_BASE_URL", "")
	cfg, err := ParseModel("openai-decisions:decider-1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "k" || cfg.BaseURL != "https://api.openai.com/v1" || cfg.Model != "decider-1" {
		t.Errorf("cfg = %+v", cfg)
	}
	if _, err := NewDecider(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIDecisionsChoiceValueKinds(t *testing.T) {
	for raw, want := range map[string]string{`"yes"`: "yes", `true`: "true", `false`: "false"} {
		if got := choiceValue(json.RawMessage(raw)); got != want {
			t.Errorf("choiceValue(%s) = %q, want %q", raw, got, want)
		}
	}
}
