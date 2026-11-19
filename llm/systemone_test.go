package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const soReplyJSON = `{"model":"decide:1","answers":{
 "refund":{"type":"noul","noul":0.9923},
 "team":{"type":"choice","choice":"payments","confidence":0.9892,"probabilities":{"payments":0.9964,"mobile":0.0018}},
 "urgency":{"type":"score","score":0.9926,"confidence":0.97,"legend":{"0":{"label":"low"},"1":{"label":"high"}},"probabilities":{"0":0.0074,"1":0.9926}}},
 "usage":{"input_tokens":142,"output_tokens":0}}`

func soQuestions() []Question {
	return []Question{
		{Name: "refund", Type: YesNo, Instructions: "Is the customer asking for a refund?"},
		{Name: "team", Type: Choice, Instructions: "Which team?", Options: []Option{
			{Value: "payments", Description: "billing, refunds"}, {Value: "mobile", Description: "app bugs"}}},
		{Name: "urgency", Type: Score, Instructions: "How urgent?", Options: []Option{
			{Value: "low", Description: "cosmetic"}, {Value: "high", Description: "outage"}}},
	}
}

func soRequest() DecisionRequest {
	return DecisionRequest{Input: []Block{TextBlock("state")}, Questions: soQuestions()}
}

func newTestSystemOne(t *testing.T, baseURL string) Decider {
	t.Helper()
	d, err := NewDecider(Config{Format: FormatSystemOne, BaseURL: baseURL, APIKey: "key-1", Model: "decide:1",
		MaxRetries: -1, RetryInterval: time.Millisecond, Headers: map[string]string{"X-Extra": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSystemOneRequestAndAnswers(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer key-1" || r.Header.Get("X-Extra") != "yes" ||
			r.Header.Get("content-type") != "application/json" {
			t.Errorf("headers: %v", r.Header)
		}
		gotBody, _ = io.ReadAll(r.Body)
		io.WriteString(w, soReplyJSON)
	}))
	defer srv.Close()

	req := soRequest()
	req.Input = []Block{TextBlock("first"), TextBlock("second")}
	resp, err := newTestSystemOne(t, srv.URL).Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"decide:1","state":"first\n\nsecond","questions":{
	 "refund":{"type":"noul","instructions":"Is the customer asking for a refund?"},
	 "team":{"type":"choice","instructions":"Which team?","criteria":{"payments":"billing, refunds","mobile":"app bugs"}},
	 "urgency":{"type":"score","instructions":"How urgent?","criteria":[{"label":"low","description":"cosmetic"},{"label":"high","description":"outage"}]}}}`
	assertJSONEq(t, gotBody, want)

	if a := resp.Answers["refund"]; a.Type != YesNo || a.Yes != 0.9923 {
		t.Errorf("refund = %+v", a)
	}
	if a := resp.Answers["team"]; a.Choice != "payments" || a.Confidence != 0.9892 || a.Probabilities["mobile"] != 0.0018 {
		t.Errorf("team = %+v", a)
	}
	u := resp.Answers["urgency"]
	if u.Score != 0.9926 || u.Confidence != 0.97 || u.Probabilities["high"] != 0.9926 || u.Probabilities["low"] != 0.0074 {
		t.Errorf("urgency = %+v", u)
	}
	if resp.Model != "decide:1" || resp.Usage.Input != 142 || resp.Usage.Output != 0 {
		t.Errorf("model/usage = %q %+v", resp.Model, resp.Usage)
	}
}

func assertJSONEq(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("request body\n got %s\nwant %s", gb, wb)
	}
}

func TestSystemOneURL(t *testing.T) {
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		io.WriteString(w, soReplyJSON)
	}))
	defer srv.Close()
	for _, suffix := range []string{"", "/", "/v1", "/v1/"} {
		path.Store("")
		if _, err := newTestSystemOne(t, srv.URL+suffix).Decide(context.Background(), soRequest()); err != nil {
			t.Fatalf("base %q: %v", suffix, err)
		}
		if path.Load() != "/v1/systemone" {
			t.Errorf("base %q hit %v", suffix, path.Load())
		}
	}
}

func TestSystemOneRejectsImageInput(t *testing.T) {
	d := newTestSystemOne(t, "http://unused.invalid")
	req := soRequest()
	req.Input = []Block{TextBlock("ok"), {Type: BlockImage}}
	_, err := d.Decide(context.Background(), req)
	if err == nil || err.Error() != "llm: systemone accepts text input only (got image)" {
		t.Errorf("err = %v", err)
	}
}

func TestSystemOneMissingAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"answers":{"refund":{"type":"noul","noul":0.5}}}`)
	}))
	defer srv.Close()
	_, err := newTestSystemOne(t, srv.URL).Decide(context.Background(), soRequest())
	if err == nil || !strings.Contains(err.Error(), `no answer for question "team"`) {
		t.Errorf("err = %v", err)
	}
}

func TestSystemOneBadScoreKey(t *testing.T) {
	for _, key := range []string{"x", "5"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"answers":{"refund":{"type":"noul","noul":0.5},
			 "team":{"type":"choice","choice":"payments"},
			 "urgency":{"type":"score","score":1,"probabilities":{"`+key+`":1}}}}`)
		}))
		_, err := newTestSystemOne(t, srv.URL).Decide(context.Background(), soRequest())
		srv.Close()
		want := `llm: systemone score key "` + key + `" for question "urgency" is not a level index`
		if err == nil || err.Error() != want {
			t.Errorf("key %q: err = %v", key, err)
		}
	}
}

func TestSystemOneRetriesThenAPIError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, soReplyJSON)
	}))
	d, err := NewDecider(Config{Format: FormatSystemOne, BaseURL: srv.URL, Model: "m", RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Decide(context.Background(), soRequest()); err != nil || hits.Load() != 2 {
		t.Errorf("err = %v, hits = %d", err, hits.Load())
	}
	srv.Close()

	hits.Store(0)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"question \"a\": type must be choice, noul, or score"}`)
	}))
	defer srv.Close()
	d, err = NewDecider(Config{Format: FormatSystemOne, BaseURL: srv.URL, Model: "m", RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Decide(context.Background(), soRequest())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest || hits.Load() != 1 {
		t.Errorf("err = %v, hits = %d", err, hits.Load())
	}
}

func TestParseModelSystemOne(t *testing.T) {
	t.Setenv("SYSTEMONE_API_KEY", "k")
	t.Setenv("SYSTEMONE_BASE_URL", "http://h")
	cfg, err := ParseModel("systemone:decide:1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "k" || cfg.BaseURL != "http://h" || cfg.Model != "decide:1" {
		t.Errorf("cfg = %+v", cfg)
	}
	if _, err := NewDecider(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.BaseURL = ""
	_, err = NewDecider(cfg)
	if err == nil || err.Error() != "llm: systemone needs a BaseURL (or SYSTEMONE_BASE_URL)" {
		t.Errorf("err = %v", err)
	}
}
