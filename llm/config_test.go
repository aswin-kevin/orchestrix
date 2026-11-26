package llm

import (
	"net/http"
	"testing"
	"time"
)

func TestParseModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "ok")
	t.Setenv("OPENAI_BASE_URL", "http://localhost:8080/v1")

	tests := []struct {
		spec    string
		format  Format
		model   string
		key     string
		baseURL string
	}{
		{"anthropic:claude-x", FormatAnthropic, "claude-x", "ak", "https://api.anthropic.com"},
		{"openai:my-model:v2", FormatOpenAI, "my-model:v2", "ok", "http://localhost:8080/v1"},
		{"openai-responses:m", FormatOpenAIResponses, "m", "ok", "http://localhost:8080/v1"},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			cfg, err := ParseModel(tt.spec)
			if err != nil {
				t.Fatalf("ParseModel: %v", err)
			}
			if cfg.Format != tt.format || cfg.Model != tt.model || cfg.APIKey != tt.key || cfg.BaseURL != tt.baseURL {
				t.Errorf("got %+v", cfg)
			}
		})
	}
}

func TestParseModelDefaultOpenAIBaseURL(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	cfg, err := ParseModel("openai:m")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
}

func TestParseModelErrors(t *testing.T) {
	for _, spec := range []string{"bogus:x", "openai", "openai:", ""} {
		if _, err := ParseModel(spec); err == nil {
			t.Errorf("ParseModel(%q) succeeded, want error", spec)
		}
	}
}

func TestNewTransportProxy(t *testing.T) {
	tr := testTransport(t, func(c *Config) { c.Proxy = "http://proxy.local:3128" })
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	got, err := tr.client.Transport.(*http.Transport).Proxy(req)
	if err != nil || got.Host != "proxy.local:3128" {
		t.Errorf("proxy = %v, %v", got, err)
	}
	if _, err := NewTransport(Config{Proxy: "http://bad host"}); err == nil {
		t.Error("invalid proxy URL should fail")
	}
}

// TestConfigNegatives pins the rule for Config's numeric fields: zero means
// the default, a field's named -1 constant is accepted with its documented
// meaning, and any other negative value fails New, NewDecider and
// NewTransport with an error naming the field.
func TestConfigNegatives(t *testing.T) {
	for _, tt := range []struct {
		name  string
		set   func(*Config)
		want  string                // "" means accepted
		check func(*Transport) bool // the accepted value's meaning
	}{
		{"zero", func(*Config) {}, "", func(tr *Transport) bool {
			return tr.maxRetries() == DefaultMaxRetries && tr.streamIdleTimeout() == DefaultStreamIdleTimeout &&
				(&openAIProvider{cfg: tr.cfg}).emptyRetries() == DefaultEmptyResponseRetries
		}},
		{"MaxRetries NoRetries", func(c *Config) { c.MaxRetries = NoRetries }, "",
			func(tr *Transport) bool { return tr.maxRetries() == 0 }},
		{"EmptyResponseRetries NoRetries", func(c *Config) { c.EmptyResponseRetries = NoRetries }, "",
			func(tr *Transport) bool { return (&openAIProvider{cfg: tr.cfg}).emptyRetries() == 0 }},
		{"StreamIdleTimeout NoIdleTimeout", func(c *Config) { c.StreamIdleTimeout = NoIdleTimeout }, "",
			func(tr *Transport) bool { return tr.streamIdleTimeout() == 0 }},
		{"MaxRetries", func(c *Config) { c.MaxRetries = -2 }, "llm: MaxRetries is negative", nil},
		{"EmptyResponseRetries", func(c *Config) { c.EmptyResponseRetries = -2 }, "llm: EmptyResponseRetries is negative", nil},
		{"StreamIdleTimeout", func(c *Config) { c.StreamIdleTimeout = -2 }, "llm: StreamIdleTimeout is negative", nil},
		{"RetryInterval", func(c *Config) { c.RetryInterval = -time.Second }, "llm: RetryInterval is negative", nil},
		{"DefaultMaxTokens", func(c *Config) { c.DefaultMaxTokens = -1 }, "llm: DefaultMaxTokens is negative", nil},
		{"RateLimit.PerSecond", func(c *Config) { c.RateLimit = &RateLimit{PerSecond: -1} }, "llm: RateLimit.PerSecond is negative", nil},
		{"RateLimit.PerMinute", func(c *Config) { c.RateLimit = &RateLimit{PerMinute: -1} }, "llm: RateLimit.PerMinute is negative", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Format: FormatOpenAI, Model: "m"}
			tt.set(&cfg)
			_, errNew := New(cfg)
			dcfg := cfg
			dcfg.Format = FormatOpenAIDecisions
			_, errDecider := NewDecider(dcfg)
			tr, errTransport := NewTransport(cfg)
			for name, err := range map[string]error{"New": errNew, "NewDecider": errDecider, "NewTransport": errTransport} {
				if got := errText(err); got != tt.want {
					t.Errorf("%s = %q, want %q", name, got, tt.want)
				}
			}
			if tt.check != nil && errTransport == nil && !tt.check(tr) {
				t.Errorf("%s does not have its documented meaning", tt.name)
			}
		})
	}
}

// errText returns err's text, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
