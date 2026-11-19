package llm_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
)

// TestConfigDoRetriesFromOutsidePackage checks that an adapter outside package
// llm gets the shared retry behaviour through Prepare and Do.
func TestConfigDoRetriesFromOutsidePackage(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := llm.Config{RetryInterval: time.Millisecond}
	if err := cfg.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	resp, err := cfg.Do(context.Background(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("got %d %q, want 200 ok", resp.StatusCode, body)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hit %d times, want 2", got)
	}
}

func TestConfigDoUnprepared(t *testing.T) {
	var cfg llm.Config
	resp, err := cfg.Do(context.Background(), func() (*http.Request, error) {
		t.Error("build called on an unprepared Config")
		return nil, errors.New("unreachable")
	})
	if resp != nil || err == nil || err.Error() != "llm: Config.Do called before Prepare" {
		t.Errorf("got %v, %v", resp, err)
	}
}
