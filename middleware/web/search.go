package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aswin-kevin/orchestrix/tool"
)

// Search tool settings.
const (
	// ToolSearch is the name of the search tool.
	ToolSearch = "web_search"
	// DefaultSearchResults is how many results a search returns when the
	// model asks for none in particular.
	DefaultSearchResults = 5
	// MaxSearchResults is the most results one search returns.
	MaxSearchResults = 10

	// maxSearchBody is how much of a search response body is read.
	maxSearchBody = 2 << 20
	// errorBodyBytes is how much of a failed response a backend error quotes.
	errorBodyBytes = 300
)

const searchDescription = "Search the web and return a numbered list of results, each with a title, a URL and a snippet. " +
	"Use web_fetch to read a result in full."

// SearchInput is the input of web_search.
type SearchInput struct {
	// Query is the search query.
	Query string `json:"query" desc:"The search query."`
	// MaxResults is the number of results, 1 to 10.
	MaxResults int `json:"max_results,omitempty" desc:"Number of results (1-10)."`
}

// searchTool returns web_search over backend. Its permission subject is the
// query; it has no verdict of its own, so a call no rule allows needs an
// approval unless readOnly is set.
func searchTool(backend SearchBackend, readOnly bool) tool.Tool {
	opts := []tool.Option{tool.WithConcurrent(), tool.WithSubject(searchSubject)}
	if readOnly {
		opts = append(opts, tool.WithReadOnly())
	}
	return tool.New(ToolSearch, searchDescription, func(ctx context.Context, in SearchInput, _ *tool.Call) (tool.Result, error) {
		return runSearch(ctx, backend, in), nil
	}, opts...)
}

// searchSubject returns the query of the input.
func searchSubject(raw json.RawMessage) string {
	var in SearchInput
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	return in.Query
}

func runSearch(ctx context.Context, backend SearchBackend, in SearchInput) tool.Result {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return tool.Errorf("query must not be empty")
	}
	n := min(positive(in.MaxResults, DefaultSearchResults), MaxSearchResults)
	results, err := backend.Search(ctx, query, n)
	if err != nil {
		return tool.Errorf("search failed: %v", err)
	}
	if len(results) > n {
		results = results[:n]
	}
	if len(results) == 0 {
		return tool.Text("No results.")
	}
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%d. %s\n   %s", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "\n   %s", r.Snippet)
		}
	}
	return tool.Text(b.String())
}

// searchClient is what the three backends share: an HTTP client, the
// User-Agent and the endpoint.
type searchClient struct {
	name      string // for error messages
	client    *http.Client
	userAgent string
	baseURL   string
}

// newSearchClient fills the optional settings of a backend config: client
// nil builds one that skips the private-address policy, because search
// endpoints are fixed by the developer and not chosen by the model.
func newSearchClient(name string, client *http.Client, userAgent, baseURL, defaultURL string) (*searchClient, error) {
	if client == nil {
		var err error
		client, err = NewHTTPClient(ClientConfig{AllowPrivateNetworks: true, UserAgent: userAgent})
		if err != nil {
			return nil, err
		}
	}
	return &searchClient{
		name:      name,
		client:    client,
		userAgent: orDefault(userAgent, DefaultUserAgent),
		baseURL:   orDefault(baseURL, defaultURL),
	}, nil
}

// do sends req and returns the body and the status. Redirects are not
// followed, so request headers such as API keys never leave the endpoint: a
// 3xx is an error like any status outside 2xx, quoting the start of the
// body. A body over maxSearchBody is an error, not partial data.
func (s *searchClient) do(req *http.Request) (body []byte, status int, err error) {
	req.Header.Set("User-Agent", s.userAgent)
	noRedirect := *s.client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %s", s.name, describe(err))
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxSearchBody+1))
	if err != nil {
		return nil, 0, fmt.Errorf("%s: reading response: %s", s.name, describe(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		quote := body[:min(len(body), errorBodyBytes)]
		return nil, 0, fmt.Errorf("%s: HTTP %d: %s", s.name, resp.StatusCode, strings.ToValidUTF8(string(quote), "�"))
	}
	if len(body) > maxSearchBody {
		return nil, 0, fmt.Errorf("%s: response exceeds %d MiB", s.name, maxSearchBody>>20)
	}
	return body, resp.StatusCode, nil
}

// clean trims the fields of results and drops those whose URL is not an
// absolute http or https URL, keeping at most n of them.
func clean(results []SearchResult, n int) []SearchResult {
	if n <= 0 {
		return nil
	}
	out := make([]SearchResult, 0, min(len(results), n))
	for _, r := range results {
		r.Title, r.URL, r.Snippet = strings.TrimSpace(r.Title), strings.TrimSpace(r.URL), strings.TrimSpace(r.Snippet)
		if u, err := url.Parse(r.URL); err != nil || !webScheme(u.Scheme) || u.Host == "" {
			continue
		}
		if out = append(out, r); len(out) == n {
			break
		}
	}
	return out
}
