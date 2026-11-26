// Package cache asks the model provider to cache the recent conversation, so
// each turn only pays for the messages added since the previous one. The
// system prompt boundary is owned by whoever builds the request; this
// middleware only controls the trailing-message breakpoints.
package cache

import (
	"context"
	"errors"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
)

// DefaultRecent is the number of trailing user messages marked as cache
// breakpoints when Config.Recent is zero.
const DefaultRecent = 2

// Name is the name of the cache middleware.
const Name = "cache"

// Config configures the cache middleware.
type Config struct {
	// Recent is the number of trailing user messages marked as cache
	// breakpoints. Zero means DefaultRecent; negative is an error.
	Recent int
}

// Middleware is the cache middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type Middleware struct {
	recent int
}

// New returns a middleware that sets llm.Request.CacheRecent on every model
// call whose model supports prompt caching and leaves other calls untouched.
// It returns an error when Recent is negative.
func New(cfg Config) (*Middleware, error) {
	if cfg.Recent < 0 {
		return nil, errors.New("cache: Recent must not be negative")
	}
	if cfg.Recent == 0 {
		cfg.Recent = DefaultRecent
	}
	return &Middleware{recent: cfg.Recent}, nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// WrapModel implements agent.ModelWrapper: for a model whose profile
// supports caching, it asks the provider to mark the request's last Recent
// user messages as cache breakpoints (llm.Request.CacheRecent).
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if req.Profile.Caching {
		req.Request.CacheRecent = m.recent
	}
	return next(ctx, req)
}
