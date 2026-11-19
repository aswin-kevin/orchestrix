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

type tavily struct {
	*searchClient
	apiKey string
}

// Tavily returns a backend that uses the Tavily search API. It fails when
// APIKey is empty.
func Tavily(cfg TavilyConfig) (SearchBackend, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("web: tavily: %w", errNoAPIKey)
	}
	sc, err := newSearchClient("tavily", cfg.Client, cfg.UserAgent, cfg.BaseURL, defaultTavilyURL)
	if err != nil {
		return nil, err
	}
	return tavily{sc, cfg.APIKey}, nil
}

// Search implements SearchBackend.
func (t tavily) Search(ctx context.Context, query string, n int) ([]SearchResult, error) {
	if n <= 0 {
		return nil, nil
	}
	payload, err := json.Marshal(map[string]any{"query": query, "max_results": n})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	body, _, err := t.do(req)
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
