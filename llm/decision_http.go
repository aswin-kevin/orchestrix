package llm

import (
	"bytes"
	"context"
	"net/http"
)

// postDecision sends body as a JSON POST to url with cfg's authorization and
// extra headers, through cfg's retrying client. The caller closes the body.
func postDecision(ctx context.Context, cfg *Config, url string, body []byte) (*http.Response, error) {
	return cfg.Do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}
		req.Header.Set("content-type", "application/json")
		for k, v := range cfg.Headers {
			req.Header.Set(k, v)
		}
		return req, nil
	})
}
