package llm

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Format names a provider wire protocol.
type Format string

// Supported wire formats.
const (
	FormatAnthropic       Format = "anthropic"
	FormatOpenAI          Format = "openai"
	FormatOpenAIResponses Format = "openai-responses"
	// FormatSystemOne and FormatOpenAIDecisions are decision formats: build
	// them with NewDecider, not New. A systemone BaseURL may end with /v1 or
	// not; the adapter adds /v1 when it is missing.
	FormatSystemOne       Format = "systemone"
	FormatOpenAIDecisions Format = "openai-decisions"
)

// Defaults applied when the corresponding Config field is zero.
const (
	// DefaultMaxRetries is the number of retries after the first attempt.
	DefaultMaxRetries = 3
	// DefaultEmptyResponseRetries is how often an empty OpenAI chat reply is
	// retried.
	DefaultEmptyResponseRetries = 2
	// DefaultMaxTokensField is the OpenAI chat request field carrying the
	// output limit.
	DefaultMaxTokensField = "max_tokens"
	// DefaultMaxTokens is the output limit used when a request sets none.
	DefaultMaxTokens = 8192

	defaultAnthropicBaseURL = "https://api.anthropic.com"
	defaultOpenAIBaseURL    = "https://api.openai.com/v1"
)

// Config configures a provider adapter.
//
// An adapter calls Prepare once in its constructor and then shares the
// resulting HTTP client and rate limiter, so a Config must not be copied
// after that point; adapters keep it by pointer.
type Config struct {
	Format     Format
	BaseURL    string
	APIKey     string
	Model      string
	APIVersion string
	// Headers are extra headers sent with every request.
	Headers map[string]string

	// HTTPClient overrides the default client; Proxy is then ignored.
	HTTPClient *http.Client
	// Proxy is a proxy URL; empty means use the environment's proxy settings.
	Proxy string

	// MaxRetries is the retry count after the first attempt: 0 selects
	// DefaultMaxRetries and a negative value disables retries. A server's
	// Retry-After is honoured, capped at one minute. No wait is started that
	// would end after the call's context deadline: the provider's error is
	// returned at once instead. Inside a fallback chain (NewFallback), an
	// entry that another follows does not retry an error that would make
	// the chain switch.
	MaxRetries int
	// RetryInterval is a fixed wait between retries; zero selects
	// exponential backoff with jitter.
	RetryInterval time.Duration
	// EmptyResponseRetries applies to OpenAI chat only: 0 selects
	// DefaultEmptyResponseRetries and a negative value disables the retry.
	EmptyResponseRetries int
	// RateLimit throttles request starts; nil means unlimited.
	RateLimit *RateLimit

	Thinking        string
	ReasoningEffort string
	// MaxTokensField names the OpenAI chat output-limit field; empty selects
	// DefaultMaxTokensField.
	MaxTokensField string
	// DefaultMaxTokens is used when a request sets no limit; 0 selects
	// DefaultMaxTokens.
	DefaultMaxTokens int

	client  *http.Client
	limiter *limiter
}

// ParseModel builds a Config from a "format:model" spec such as
// "openai:my-model:v2". Only the first colon separates the format, so model
// names may contain colons. The API key and base URL come from the format's
// environment variables.
func ParseModel(spec string) (Config, error) {
	prefix, model, ok := strings.Cut(spec, ":")
	if !ok || model == "" {
		return Config{}, fmt.Errorf("llm: model spec %q must look like format:model", spec)
	}
	cfg := Config{Format: Format(prefix), Model: model}
	if _, ok := lookup(cfg.Format); !ok {
		return Config{}, fmt.Errorf("llm: unknown provider format %q in model spec %q", prefix, spec)
	}
	switch cfg.Format {
	case FormatAnthropic:
		cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		cfg.BaseURL = envOr("ANTHROPIC_BASE_URL", defaultAnthropicBaseURL)
	case FormatOpenAI, FormatOpenAIResponses, FormatOpenAIDecisions:
		cfg.APIKey = os.Getenv("OPENAI_API_KEY")
		cfg.BaseURL = envOr("OPENAI_BASE_URL", defaultOpenAIBaseURL)
	case FormatSystemOne:
		cfg.APIKey = os.Getenv("SYSTEMONE_API_KEY")
		cfg.BaseURL = os.Getenv("SYSTEMONE_BASE_URL")
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// Prepare builds the HTTP client and rate limiter. Call it once before Do
// (adapters do so from their constructor); it is not safe to call
// concurrently with Do.
func (c *Config) Prepare() error {
	client, err := c.newHTTPClient()
	if err != nil {
		return err
	}
	c.client = client
	c.limiter = newLimiter(c.RateLimit)
	return nil
}

// httpClient returns the client built by Prepare.
func (c *Config) httpClient() *http.Client {
	return c.client
}

func (c *Config) newHTTPClient() (*http.Client, error) {
	if c.HTTPClient != nil {
		return c.HTTPClient, nil
	}
	transport := &http.Transport{}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = def.Clone()
	}
	if c.Proxy != "" {
		proxyURL, err := url.Parse(c.Proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: parse proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	if c.Proxy == "" {
		transport.Proxy = http.ProxyFromEnvironment
	}
	// No overall client timeout: streams are
	// long-lived and callers bound them with a context.
	return &http.Client{Transport: transport}, nil
}

// New returns the Provider for cfg.Format. The configuration is copied, so
// the caller may reuse or modify cfg afterwards.
func New(cfg Config) (Provider, error) {
	if cfg.Model == "" {
		return nil, errors.New("llm: config has no model")
	}
	e, ok := lookup(cfg.Format)
	if !ok {
		return nil, fmt.Errorf("llm: unknown provider format %q", cfg.Format)
	}
	if e.provider == nil {
		return nil, fmt.Errorf("llm: format %q is a decision format; use NewDecider", cfg.Format)
	}
	p, err := e.provider(cfg)
	if err != nil {
		return nil, err
	}
	return p, nil
}
