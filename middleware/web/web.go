// Package web gives the agent the web: the web_fetch tool reads a page and
// returns it as markdown, and a SearchBackend can add web search.
//
// Every request goes through a client that guards against server-side
// request forgery: unless Config.AllowPrivateNetworks is set, connections to
// loopback, private, link-local and other non-public addresses are refused
// at connect time, for every redirect hop and every DNS answer. Pages are
// model input from an untrusted source; the default prompt says so.
package web

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// ToolFetch is the name of the fetch tool.
const ToolFetch = "web_fetch"

// Defaults for the zero values of Config and ClientConfig.
const (
	// DefaultMaxBytes is how much of a response body is read.
	DefaultMaxBytes = 5 << 20
	// DefaultMaxChars is how many characters of text one call returns.
	DefaultMaxChars = 50_000
	// DefaultTimeout bounds a whole request, body included.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxRedirects is how many redirects a request follows.
	DefaultMaxRedirects = 5
	// DefaultUserAgent is the User-Agent of requests that set none.
	DefaultUserAgent = "Mozilla/5.0 (compatible; orchestrix-web/0.3)"
)

// ErrBlockedAddress is wrapped by the errors of requests the network guard
// refuses.
var ErrBlockedAddress = errors.New("web: blocked address")

// SearchBackend runs web searches for the search tool. Implementations must
// be safe for concurrent use. A backend that also implements Validator is
// validated by New.
type SearchBackend interface {
	// Search returns at most n results for query, best first. n is at
	// least 1. An empty result with a nil error means nothing was found.
	Search(ctx context.Context, query string, n int) ([]SearchResult, error)
}

// SearchResult is one hit of a web search.
type SearchResult struct {
	// Title is the page title; empty when the backend has none.
	Title string
	// URL is the absolute http or https URL of the page; never empty.
	URL string
	// Snippet is a short excerpt of the page; empty when the backend has
	// none.
	Snippet string
}

// Validator is optionally implemented by a SearchBackend to report a broken
// configuration (a missing key, an invalid endpoint) when the middleware is
// built rather than on the first search.
type Validator interface {
	// Validate returns nil when the backend is ready to search.
	Validate() error
}

// Config configures the web middleware.
type Config struct {
	// DisableFetch leaves out web_fetch. False offers it.
	DisableFetch bool
	// Search is the web search backend. Nil offers no search tool.
	Search SearchBackend

	// MaxBytes is how much of a response body web_fetch reads; ≤ 0 means
	// DefaultMaxBytes.
	MaxBytes int64
	// MaxChars is how many characters of text one web_fetch call returns
	// at most, whatever the model asks for; ≤ 0 means DefaultMaxChars.
	MaxChars int
	// Timeout bounds each request, body included; ≤ 0 means
	// DefaultTimeout.
	Timeout time.Duration
	// MaxRedirects is how many redirects a request follows; ≤ 0 means
	// DefaultMaxRedirects.
	MaxRedirects int

	// AllowPrivateNetworks permits loopback, private, link-local, CGNAT and
	// other non-public addresses. False refuses them.
	AllowPrivateNetworks bool
	// ProxyURL is the http or https proxy for fetch and search. Empty means
	// no proxy; the proxy environment variables are always ignored.
	ProxyURL string
	// CACertFiles are PEM files of extra root certificates, added to the
	// system pool. Nil trusts the system pool only.
	CACertFiles []string
	// UserAgent is sent with every request; empty means DefaultUserAgent.
	UserAgent string

	// ReadOnly reports the web tools as read-only, so permission checks
	// allow them without a rule. False makes them need an allow rule or an
	// approval.
	ReadOnly bool
	// Prompt is the system prompt segment added while web tools are
	// offered. nil uses DefaultPrompt, plus a line about web_search when
	// Search is set; any other value replaces all of that, so the host
	// then describes web_search itself. A pointer to "" disables the
	// segment.
	Prompt *string
}

// ClientConfig returns the HTTP client settings of c, for building search
// backends whose requests follow the same network policy as web_fetch.
func (c Config) ClientConfig() ClientConfig {
	return ClientConfig{
		Timeout:              c.Timeout,
		MaxRedirects:         c.MaxRedirects,
		ProxyURL:             c.ProxyURL,
		CACertFiles:          c.CACertFiles,
		AllowPrivateNetworks: c.AllowPrivateNetworks,
		UserAgent:            c.UserAgent,
	}
}

// middleware is the web middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type middleware struct {
	tools  []tool.Tool
	prompt string
}

// New returns the web middleware. It fails when ProxyURL is not an http or
// https URL, when a CA file cannot be read or holds no PEM certificate, when
// the Search backend fails validation, and when nothing is enabled (fetch
// disabled and no Search).
func New(cfg Config) (agent.Middleware, error) {
	if cfg.DisableFetch && cfg.Search == nil {
		return nil, errors.New("web: nothing enabled: fetch is disabled and no Search backend is set")
	}
	if v, ok := cfg.Search.(Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, fmt.Errorf("web: search backend: %w", err)
		}
	}
	client, err := NewHTTPClient(cfg.ClientConfig())
	if err != nil {
		return nil, err
	}
	m := &middleware{prompt: DefaultPrompt}
	if cfg.Search != nil {
		m.prompt += searchPrompt
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	if !cfg.DisableFetch {
		m.tools = append(m.tools, newFetcher(client, cfg).tool(cfg.ReadOnly))
	}
	if cfg.Search != nil {
		m.tools = append(m.tools, searchTool(cfg.Search, cfg.ReadOnly))
	}
	return m, nil
}

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "web" }

// Tools implements agent.ToolProvider.
func (m *middleware) Tools() []tool.Tool { return slices.Clone(m.tools) }

// WrapModel implements agent.ModelWrapper. It appends the prompt after the
// host's system segments, outside the cached prefix.
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if m.prompt != "" && len(m.tools) > 0 {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt)
	}
	return next(ctx, req)
}
