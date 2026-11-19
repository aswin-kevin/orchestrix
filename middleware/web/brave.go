package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const defaultBraveURL = "https://api.search.brave.com/res/v1/web/search"

// BraveConfig configures the Brave Search backend.
type BraveConfig struct {
	// APIKey is the subscription token. It is required.
	APIKey string
	// BaseURL is the search endpoint; "" means
	// "https://api.search.brave.com/res/v1/web/search".
	BaseURL string
	// Client sends the requests. Nil builds a client without the
	// private-address policy of web_fetch; see DuckDuckGoConfig.Client for
	// passing one with a proxy or extra roots.
	Client *http.Client
	// UserAgent is sent with every request; "" means DefaultUserAgent.
	UserAgent string
}

type brave struct {
	*searchClient
	apiKey string
}

// Brave returns a backend that uses the Brave Search API. It fails when
// APIKey is empty.
func Brave(cfg BraveConfig) (SearchBackend, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("web: brave: %w", errNoAPIKey)
	}
	sc, err := newSearchClient("brave", cfg.Client, cfg.UserAgent, cfg.BaseURL, defaultBraveURL)
	if err != nil {
		return nil, err
	}
	return brave{sc, cfg.APIKey}, nil
}

// Search implements SearchBackend.
func (b brave) Search(ctx context.Context, query string, n int) ([]SearchResult, error) {
	q := url.Values{"q": {query}, "count": {strconv.Itoa(n)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Subscription-Token", b.apiKey)
	req.Header.Set("Accept", "application/json")
	body, err := b.do(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("brave: invalid response: %w", err)
	}
	results := make([]SearchResult, len(out.Web.Results))
	for i, r := range out.Web.Results {
		results[i] = SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Description}
	}
	return clean(results, n), nil
}
