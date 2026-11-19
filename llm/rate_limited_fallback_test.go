package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// A rate-limited primary should hand over to a healthy backup promptly,
// not only after its own retries, which honor Retry-After.
func TestRateLimitedPrimaryFallsBackPromptly(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "20")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()

	req := llm.Request{Model: "claude-sonnet-4-5", MaxTokens: 100, Messages: []llm.Message{llm.UserText("hi")}}
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			hits.Store(0)
			primary, err := llm.New(llm.Config{Format: llm.FormatAnthropic, Model: "claude-sonnet-4-5", APIKey: "test", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			fb, err := llm.NewFallback(llm.FallbackConfig{Entries: []llm.FallbackEntry{
				{Provider: primary, Name: "primary"},
				{Provider: llmtest.New(llmtest.Turn{Text: "from backup"}), Name: "backup"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			var text strings.Builder
			if mode == "complete" {
				resp, err := fb.Complete(ctx, req)
				if err != nil {
					t.Fatalf("after %v and %d primary attempts: %v", time.Since(start).Round(time.Second), hits.Load(), err)
				}
				text.WriteString(resp.Message.Text())
			} else {
				for ev, err := range fb.Stream(ctx, req) {
					if err != nil {
						t.Fatalf("after %v and %d primary attempts: %v", time.Since(start).Round(time.Second), hits.Load(), err)
					}
					if ev.Type == llm.EvTextDelta {
						text.WriteString(ev.Text)
					}
				}
			}
			if text.String() != "from backup" {
				t.Fatalf("answer = %q, want the backup's", text.String())
			}
		})
	}
}

// The last entry of a chain keeps its normal retries, so a chain never gives
// up sooner than a single provider would.
func TestFallbackLastEntryKeepsRetries(t *testing.T) {
	var primaryHits, backupHits atomic.Int32
	failing := func(hits *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
	}
	p, b := failing(&primaryHits), failing(&backupHits)
	defer p.Close()
	defer b.Close()

	primary, err := llm.New(llm.Config{Format: llm.FormatAnthropic, Model: "claude-sonnet-4-5", APIKey: "test", BaseURL: p.URL, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := llm.New(llm.Config{Format: llm.FormatAnthropic, Model: "claude-sonnet-4-5", APIKey: "test", BaseURL: b.URL, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	fb, err := llm.NewFallback(llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: primary, Name: "primary"},
		{Provider: backup, Name: "backup"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{MaxTokens: 100, Messages: []llm.Message{llm.UserText("hi")}}
	if _, err := fb.Complete(context.Background(), req); err == nil {
		t.Fatal("want an error when every entry fails")
	}
	if got := primaryHits.Load(); got != 1 {
		t.Errorf("primary hits = %d, want 1 (hands off without retrying)", got)
	}
	if got := backupHits.Load(); got != 3 {
		t.Errorf("backup hits = %d, want 3 (first attempt plus MaxRetries)", got)
	}
}
