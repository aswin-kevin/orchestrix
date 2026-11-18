package llm

import (
	"net/http"
	"testing"
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

func TestPrepareProxy(t *testing.T) {
	cfg := &Config{Proxy: "http://proxy.local:3128"}
	if err := cfg.prepare(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	got, err := cfg.httpClient().Transport.(*http.Transport).Proxy(req)
	if err != nil || got.Host != "proxy.local:3128" {
		t.Errorf("proxy = %v, %v", got, err)
	}
	if err := (&Config{Proxy: "http://bad host"}).prepare(); err == nil {
		t.Error("invalid proxy URL should fail")
	}
}
