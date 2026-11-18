package cache

import (
	"context"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
)

func wrap(t *testing.T, cfg Config, caching bool, in llm.Request) llm.Request {
	t.Helper()
	m, ok := New(cfg).(agent.ModelWrapper)
	if !ok {
		t.Fatal("cache middleware does not wrap model calls")
	}
	var seen llm.Request
	req := &agent.ModelRequest{Request: in, Profile: llm.Profile{Caching: caching}}
	_, err := m.WrapModel(context.Background(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
		seen = r.Request
		return &llm.Response{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return seen
}

func TestName(t *testing.T) {
	if got := New(Config{}).Name(); got != "cache" {
		t.Fatalf("Name() = %q, want cache", got)
	}
}

func TestCacheMiddlewareNoopWithoutCaching(t *testing.T) {
	in := llm.Request{System: []string{"s"}, CacheBoundary: 1}
	if got := wrap(t, Config{}, false, in); got.CacheRecent != 0 || got.CacheBoundary != 1 {
		t.Fatalf("request changed without Caching: %+v", got)
	}
}

func TestCacheMiddlewareSetsRecentOnly(t *testing.T) {
	in := llm.Request{System: []string{"s"}}
	got := wrap(t, Config{}, true, in)
	if got.CacheRecent != DefaultRecent {
		t.Errorf("CacheRecent = %d, want %d", got.CacheRecent, DefaultRecent)
	}
	if got.CacheBoundary != 0 {
		t.Errorf("CacheBoundary = %d, want it left to the preset", got.CacheBoundary)
	}
}

func TestCacheMiddlewareCustomRecent(t *testing.T) {
	if got := wrap(t, Config{Recent: 3}, true, llm.Request{}); got.CacheRecent != 3 {
		t.Fatalf("CacheRecent = %d, want 3", got.CacheRecent)
	}
}
