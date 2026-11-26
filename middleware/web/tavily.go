package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const defaultTavilyURL = "https://api.tavily.com/search"

// TavilyConfig configures the Tavily backend.
type TavilyConfig struct {
	// APIKey is the bearer token. It is required.
	APIKey string
	// BaseURL is the search endpoint; "" means
	// "https://api.tavily.com/search".
	BaseURL string
	// Client sends the requests. Nil builds a client without the
	// private-address policy of web_fetch; see DuckDuckGoConfig.Client for
	// passing one with a proxy or extra roots.
	Client *http.Client
	// UserAgent is sent with every request; "" means DefaultUserAgent.
	UserAgent string
}

// Tavily is a SearchBackend that uses the Tavily search API. It is safe
// for concurrent use.
type Tavily struct {
	sc     *searchClient
	apiKey string
}

// NewTavily returns a Tavily backend. It fails with an error wrapping
// ErrNoAPIKey when APIKey is empty.
func NewTavily(cfg TavilyConfig) (*Tavily, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("%w: TavilyConfig.APIKey", ErrNoAPIKey)
	}
	sc, err := newSearchClient("tavily", cfg.Client, cfg.UserAgent, cfg.BaseURL, defaultTavilyURL)
	if err != nil {
		return nil, err
	}
	return &Tavily{sc: sc, apiKey: cfg.APIKey}, nil
}

// Search implements SearchBackend.
func (t *Tavily) Search(ctx context.Context, query string, n int) ([]SearchResult, error) {
	if n <= 0 {
		return nil, nil
	}
	payload, err := json.Marshal(map[string]any{"query": query, "max_results": n})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.sc.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	body, _, err := t.sc.do(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("tavily: invalid response: %w", err)
	}
	results := make([]SearchResult, len(out.Results))
	for i, r := range out.Results {
		results[i] = SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content}
	}
	return clean(results, n), nil
}
