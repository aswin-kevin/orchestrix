// Package web gives the agent the web: the web_fetch tool reads a page and
// returns it as markdown, and a SearchBackend can add web search.
//
// web_fetch reads pages through a Fetcher. The default, HTTPFetcher, guards
// against server-side request forgery: unless Config.AllowPrivateNetworks is
// set, connections to loopback, private, link-local and other non-public
// addresses are refused at connect time, for every redirect hop and every
// DNS answer. A custom Config.Fetcher owns its network policy instead. Pages
// are model input from an untrusted source; the default prompt says so.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/version"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Name is the name of the web middleware.
const Name = "web"

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
	DefaultUserAgent = "Mozilla/5.0 (compatible; orchestrix-web/" + version.Version + ")"
)

// ErrBlockedAddress is wrapped by the errors of requests the network guard
// refuses.
var ErrBlockedAddress = errors.New("web: blocked address")

// ErrNoAPIKey reports a search backend config without its API key: NewBrave
// and NewTavily return an error wrapping it when APIKey is empty.
var ErrNoAPIKey = errors.New("web: API key is empty")

// errFetcherExcludesClient reports a Config that sets Fetcher together with
// settings only the default HTTPFetcher uses.
var errFetcherExcludesClient = errors.New("web: Fetcher excludes the HTTP client settings")

// Page is what a Fetcher read.
type Page struct {
	// URL is the final URL, after redirects; empty means the URL
	// requested.
	URL string
	// Status is the HTTP status; 0 when the source has none. A status of
	// 400 or more makes web_fetch return an error quoting Text.
	Status int
	// Title is the page title; empty when it has none.
	Title string
	// Text is the content: markdown for HTML, the body for textual types.
	Text string
	// Truncated reports that the source was cut at maxBytes; web_fetch
	// then notes it after Text.
	Truncated bool
}

// Fetcher reads the page at a URL for web_fetch. Implementations must be
// safe for concurrent use.
//
// A custom Fetcher owns its network policy, server-side request forgery
// included: the private-address guard lives in HTTPFetcher only, so a
// Fetcher that reaches the network must refuse internal addresses itself
// when the model must not reach them.
type Fetcher interface {
	// Fetch reads u, reading at most maxBytes of its content (maxBytes is
	// at least 1). ctx carries web_fetch's timeout. A status of 400 or
	// more is a Page with that Status, not an error; an error means no
	// page was read.
	Fetch(ctx context.Context, u *url.URL, maxBytes int64) (*Page, error)
}

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
	// Fetcher reads the pages of web_fetch. Nil means an HTTPFetcher built
	// from the HTTP settings below. A custom Fetcher owns its network
	// policy (the private-address guard is HTTPFetcher's) and excludes
	// MaxRedirects, AllowPrivateNetworks, ProxyURL, CACertFiles and
	// UserAgent; Timeout, MaxBytes and MaxChars still apply.
	Fetcher Fetcher

	// MaxBytes is how much of a response body web_fetch reads; 0 means
	// DefaultMaxBytes. Negative is an error.
	MaxBytes int64
	// MaxChars is how many characters of text one web_fetch call returns
	// at most, whatever the model asks for; 0 means DefaultMaxChars.
	// Negative is an error.
	MaxChars int
	// The HTTP settings below apply to web_fetch only, and all but Timeout
	// only to the default HTTPFetcher. A search backend sends its requests
	// with the Client of its own config; to give it the same policy, build
	// that client with NewHTTPClient(cfg.ClientConfig()).

	// Timeout bounds each web_fetch request, body and conversion
	// included, with a custom Fetcher too; 0 means DefaultTimeout.
	// Negative is an error.
	Timeout time.Duration
	// MaxRedirects is how many redirects a web_fetch request follows; 0
	// means DefaultMaxRedirects. Negative is an error.
	MaxRedirects int

	// AllowPrivateNetworks lets web_fetch reach loopback, private,
	// link-local, CGNAT and other non-public addresses. False refuses them.
	AllowPrivateNetworks bool
	// ProxyURL is the http or https proxy web_fetch goes through. Empty
	// means no proxy; the proxy environment variables are always ignored.
	ProxyURL string
	// CACertFiles are PEM files of extra root certificates web_fetch
	// trusts, added to the system pool. Nil trusts the system pool only.
	CACertFiles []string
	// UserAgent is sent with every web_fetch request that sets none; empty
	// means DefaultUserAgent.
	UserAgent string

	// ReadOnly reports the web tools as read-only, so permission checks
	// allow them without a rule. False makes them need an allow rule or an
	// approval.
	ReadOnly bool
	// Prompt is the system prompt segment added while web tools are
	// offered. nil describes the tools offered: DefaultPrompt (or, with
	// DisableFetch, a variant that does not mention web_fetch), plus a line
	// about web_search when Search is set. Any other value replaces all of
	// that, so the host then describes the tools itself. A pointer to ""
	// disables the segment.
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

// Middleware is the web middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type Middleware struct {
	tools  []tool.Tool
	prompt string
}

// New returns the web middleware. It fails when MaxBytes, MaxChars, Timeout
// or MaxRedirects is negative, when Fetcher is set together with an HTTP
// client setting, when ProxyURL is not an http or https URL, when a CA file
// cannot be read or holds no PEM certificate, when the Search backend fails
// validation, and when nothing is enabled (fetch disabled and no Search).
func New(cfg Config) (*Middleware, error) {
	switch {
	case cfg.MaxBytes < 0:
		return nil, errors.New("web: MaxBytes must not be negative")
	case cfg.MaxChars < 0:
		return nil, errors.New("web: MaxChars must not be negative")
	}
	if err := checkClientLimits(cfg.Timeout, cfg.MaxRedirects); err != nil {
		return nil, err
	}
	if cfg.DisableFetch && cfg.Search == nil {
		return nil, errors.New("web: nothing enabled: fetch is disabled and no Search backend is set")
	}
	src := cfg.Fetcher
	if src != nil {
		if cfg.MaxRedirects != 0 || cfg.AllowPrivateNetworks || cfg.ProxyURL != "" || cfg.CACertFiles != nil || cfg.UserAgent != "" {
			return nil, errFetcherExcludesClient
		}
	}
	if v, ok := cfg.Search.(Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, fmt.Errorf("web: search backend: %w", err)
		}
	}
	if src == nil {
		hf, err := NewHTTPFetcher(cfg.ClientConfig())
		if err != nil {
			return nil, err
		}
		src = hf
	}
	m := &Middleware{prompt: DefaultPrompt}
	if cfg.DisableFetch {
		m.prompt = searchOnlyPrompt
	}
	if cfg.Search != nil {
		m.prompt += searchPrompt
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	if !cfg.DisableFetch {
		m.tools = append(m.tools, newFetchTool(src, cfg).tool(cfg.ReadOnly))
	}
	if cfg.Search != nil {
		m.tools = append(m.tools, searchTool(cfg.Search, cfg.ReadOnly))
	}
	return m, nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Tools implements agent.ToolProvider.
func (m *Middleware) Tools() []tool.Tool { return slices.Clone(m.tools) }

// WrapModel implements agent.ModelWrapper. It appends the prompt after the
// host's system segments, outside the cached prefix.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if m.prompt != "" && len(m.tools) > 0 {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt)
	}
	return next(ctx, req)
}
