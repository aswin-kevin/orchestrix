package orchestrix_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/llm"
)

func TestNewDeciderFromSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer key-1" {
			t.Errorf("got %s with %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"model":"decide:1","answers":{"refund":{"type":"noul","noul":0.9}},"usage":{"input_tokens":3}}`)
	}))
	defer srv.Close()
	t.Setenv("SYSTEMONE_API_KEY", "key-1")
	t.Setenv("SYSTEMONE_BASE_URL", srv.URL)

	d, err := orchestrix.NewDecider("systemone:decide:1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := d.Decide(t.Context(), llm.DecisionRequest{
		Input:     []llm.Block{llm.TextBlock("I want my money back")},
		Questions: []llm.Question{{Name: "refund", Type: llm.QuestionYesNo, Instructions: "Is this a refund request?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Answers["refund"].Yes; got != 0.9 {
		t.Errorf("refund yes = %v, want 0.9", got)
	}
}

func TestNewDeciderRejectsTextFormat(t *testing.T) {
	_, err := orchestrix.NewDecider("openai:m")
	if err == nil || !strings.Contains(err.Error(), "use New") {
		t.Fatalf("err = %v, want one mentioning use New", err)
	}
}

func TestNewDeciderRejectsBadSpec(t *testing.T) {
	if _, err := orchestrix.NewDecider("nocolon"); err == nil {
		t.Fatal("want error for a spec without a format")
	}
}
