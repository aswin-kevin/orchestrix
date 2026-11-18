// Package cache asks the model provider to cache the recent conversation, so
// each turn only pays for the messages added since the previous one. The
// system prompt boundary is owned by whoever builds the request; this
// middleware only controls the trailing-message breakpoints.
package cache

import (
	"context"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
)

// DefaultRecent is the number of trailing user messages marked as cache
// breakpoints when Config.Recent is zero.
const DefaultRecent = 2

// Config configures the cache middleware.
type Config struct {
	// Recent is the number of trailing user messages marked as cache
	// breakpoints. Zero means DefaultRecent.
	Recent int
}

type middleware struct {
	recent int
}

// New returns a middleware that sets llm.Request.CacheRecent on every model
// call whose model supports prompt caching and leaves other calls untouched.
func New(cfg Config) agent.Middleware {
	if cfg.Recent == 0 {
		cfg.Recent = DefaultRecent
	}
	return &middleware{recent: cfg.Recent}
}

func (m *middleware) Name() string { return "cache" }

func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if req.Profile.Caching {
		req.Request.CacheRecent = m.recent
	}
	return next(ctx, req)
}
