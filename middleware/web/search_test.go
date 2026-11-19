package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

const ddgFixture = `<html><body>
<div class="result results_links result--ad"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fads.example%2Fbuy">Sponsored</a><a class="result__snippet">buy now</a></div>
<div class="result results_links web-result">
  <h2><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%3Fa%3D1%26b%3D2&amp;rut=abc">
     The Go   Programming Language </a></h2>
  <a class="result__snippet" href="x">
     Go is an open source language.
  </a>
</div>
<div class="result"><a class="result__a" href="//example.org/plain">Plain protocol-relative</a><div class="result__snippet">Second <b>snippet</b></div></div>
<div class="result"><a class="result__a" href="https://example.net/direct">Direct</a></div>
<div class="result"><a class="result__a" href="javascript:void(0)">Script</a></div>
</body></html>`

type captured struct {
	mu     sync.Mutex
	method string
	header http.Header
	query  url.Values
	body   string
}

func (c *captured) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.method, c.header, c.query, c.body = r.Method, r.Header.Clone(), r.URL.Query(), string(b)
}

func backendServer(t *testing.T, status int, contentType, body string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestDuckDuckGoParses(t *testing.T) {
	srv, got := backendServer(t, 200, "text/html", ddgFixture)
	b := must(DuckDuckGo(DuckDuckGoConfig{BaseURL: srv.URL, UserAgent: "test-agent"}))
	res, err := b.Search(context.Background(), "golang & more", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []SearchResult{
		{"The Go Programming Language", "https://go.dev/doc?a=1&b=2", "Go is an open source language."},
		{"Plain protocol-relative", "https://example.org/plain", "Second snippet"},
		{"Direct", "https://example.net/direct", ""},
	}
	if fmt.Sprint(res) != fmt.Sprint(want) {
		t.Fatalf("results = %v\nwant %v", res, want)
	}
	if got.method != http.MethodPost || got.header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("method = %s, content type = %s", got.method, got.header.Get("Content-Type"))
	}
	if form, _ := url.ParseQuery(got.body); form.Get("q") != "golang & more" {
		t.Fatalf("body = %q", got.body)
	}
	if got.header.Get("User-Agent") != "test-agent" {
		t.Fatalf("user agent = %q", got.header.Get("User-Agent"))
	}
}

func TestDuckDuckGoLimitsResults(t *testing.T) {
	srv, _ := backendServer(t, 200, "text/html", ddgFixture)
	res, err := must(DuckDuckGo(DuckDuckGoConfig{BaseURL: srv.URL})).Search(context.Background(), "q", 2)
	if err != nil || len(res) != 2 {
		t.Fatalf("results = %v, err = %v", res, err)
	}
}

func TestBraveRequestAndParse(t *testing.T) {
	srv, got := backendServer(t, 200, "application/json", `{"web":{"results":[
		{"title":" One ","url":"https://one.example/","description":" first "},
		{"title":"Bad","url":"ftp://bad.example/","description":"dropped"},
		{"title":"Two","url":"https://two.example/","description":"second"}]}}`)
	b := must(Brave(BraveConfig{APIKey: "k", BaseURL: srv.URL}))
	res, err := b.Search(context.Background(), "go lang", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []SearchResult{{"One", "https://one.example/", "first"}, {"Two", "https://two.example/", "second"}}
	if fmt.Sprint(res) != fmt.Sprint(want) {
		t.Fatalf("results = %v, want %v", res, want)
	}
	if got.method != http.MethodGet || got.query.Get("q") != "go lang" || got.query.Get("count") != "3" {
		t.Fatalf("method = %s, query = %v", got.method, got.query)
	}
	if got.header.Get("X-Subscription-Token") != "k" || got.header.Get("Accept") != "application/json" {
		t.Fatalf("headers = %v", got.header)
	}
	if got.header.Get("User-Agent") != DefaultUserAgent {
		t.Fatalf("user agent = %q", got.header.Get("User-Agent"))
	}
}

func TestTavilyRequestAndParse(t *testing.T) {
	srv, got := backendServer(t, 200, "application/json", `{"results":[
		{"title":"A","url":"https://a.example/","content":" about a "},
		{"title":"B","url":"/relative","content":"dropped"}]}`)
	b := must(Tavily(TavilyConfig{APIKey: "secret", BaseURL: srv.URL}))
	res, err := b.Search(context.Background(), "query", 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := []SearchResult{{"A", "https://a.example/", "about a"}}; fmt.Sprint(res) != fmt.Sprint(want) {
		t.Fatalf("results = %v, want %v", res, want)
	}
	if got.method != http.MethodPost || got.header.Get("Authorization") != "Bearer secret" {
		t.Fatalf("method = %s, authorization = %q", got.method, got.header.Get("Authorization"))
	}
	var body struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(got.body), &body); err != nil || body.Query != "query" || body.MaxResults != 4 {
		t.Fatalf("body = %q (%v)", got.body, err)
	}
}

func TestSearchHTTPErrorStatus(t *testing.T) {
	long := strings.Repeat("x", 1000)
	build := map[string]func(url string) (SearchBackend, error){
		"duckduckgo": func(u string) (SearchBackend, error) { return DuckDuckGo(DuckDuckGoConfig{BaseURL: u}) },
		"brave":      func(u string) (SearchBackend, error) { return Brave(BraveConfig{APIKey: "k", BaseURL: u}) },
		"tavily":     func(u string) (SearchBackend, error) { return Tavily(TavilyConfig{APIKey: "k", BaseURL: u}) },
	}
	for name, mk := range build {
		t.Run(name, func(t *testing.T) {
			srv, _ := backendServer(t, 429, "text/plain", long)
			b, err := mk(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = b.Search(context.Background(), "q", 5)
			if err == nil || !strings.Contains(err.Error(), "HTTP 429") || !strings.HasPrefix(err.Error(), name+":") {
				t.Fatalf("err = %v", err)
			}
			if len(err.Error()) > 350 {
				t.Fatalf("error quotes too much: %d bytes", len(err.Error()))
			}
		})
	}
}

func TestSearchBodyIsCapped(t *testing.T) {
	srv, _ := backendServer(t, 200, "application/json", `{"results":[{"title":"A","url":"https://a.example/","content":"`+strings.Repeat("y", maxSearchBody)+`"}]}`)
	_, err := must(Tavily(TavilyConfig{APIKey: "k", BaseURL: srv.URL})).Search(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "invalid response") {
		t.Fatalf("err = %v, want truncated body to fail to decode", err)
	}
}

func TestSearchUsesProxy(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.String())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"title":"A","url":"https://a.example/","content":"c"}]}`)
	}))
	defer proxy.Close()
	client, err := NewHTTPClient(ClientConfig{ProxyURL: proxy.URL, AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	// The endpoint is never reachable directly; only the proxy answers.
	b := must(Tavily(TavilyConfig{APIKey: "k", BaseURL: "http://search.invalid/search", Client: client}))
	res, err := b.Search(context.Background(), "q", 3)
	if err != nil || len(res) != 1 {
		t.Fatalf("results = %v, err = %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "POST http://search.invalid/search" {
		t.Fatalf("proxy saw %v", seen)
	}
}

func TestMissingAPIKeyFailsNew(t *testing.T) {
	if _, err := Brave(BraveConfig{}); !errors.Is(err, errNoAPIKey) {
		t.Fatalf("Brave err = %v", err)
	}
	if _, err := Tavily(TavilyConfig{}); !errors.Is(err, errNoAPIKey) {
		t.Fatalf("Tavily err = %v", err)
	}
}

// fakeBackend records the n of its last search.
type fakeBackend struct {
	results []SearchResult
	err     error
	gotN    int
	gotQ    string
}

func (f *fakeBackend) Search(_ context.Context, q string, n int) ([]SearchResult, error) {
	f.gotQ, f.gotN = q, n
	return f.results, f.err
}

func TestSearchToolFormatsAndClamps(t *testing.T) {
	call := func(f *fakeBackend, input string) tool.Result {
		t.Helper()
		mw, err := New(Config{DisableFetch: true, Search: f})
		if err != nil {
			t.Fatal(err)
		}
		res, err := mw.(agent.ToolProvider).Tools()[0].Call(context.Background(), json.RawMessage(input), &tool.Call{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	f := &fakeBackend{results: []SearchResult{{"First", "https://a.example/", "about a"}, {"Second", "https://b.example/", ""}}}
	res := call(f, `{"query":"  go  "}`)
	if want := "1. First\n   https://a.example/\n   about a\n\n2. Second\n   https://b.example/"; res.Flatten() != want || res.IsError {
		t.Fatalf("output = %q (error %v)", res.Flatten(), res.IsError)
	}
	if f.gotQ != "go" {
		t.Fatalf("query = %q", f.gotQ)
	}
	for input, want := range map[string]int{
		`{"query":"q","max_results":50}`: MaxSearchResults,
		`{"query":"q"}`:                  DefaultSearchResults,
		`{"query":"q","max_results":-3}`: DefaultSearchResults,
		`{"query":"q","max_results":2}`:  2,
	} {
		call(f, input)
		if f.gotN != want {
			t.Errorf("%s: n = %d, want %d", input, f.gotN, want)
		}
	}
	if res := call(&fakeBackend{}, `{"query":"q"}`); res.Flatten() != "No results." || res.IsError {
		t.Fatalf("empty = %q", res.Flatten())
	}
	if res := call(&fakeBackend{err: errors.New("boom")}, `{"query":"q"}`); !res.IsError || !strings.Contains(res.Flatten(), "boom") {
		t.Fatalf("error result = %q (error %v)", res.Flatten(), res.IsError)
	}
	if res := call(f, `{"query":"  "}`); !res.IsError {
		t.Fatalf("blank query = %q", res.Flatten())
	}
}

func TestSearchToolAttributes(t *testing.T) {
	tl := searchTool(&fakeBackend{}, false)
	in := json.RawMessage(`{"query":"find me"}`)
	if tl.ReadOnly(in) || !tl.ConcurrencySafe(in) {
		t.Fatalf("readOnly = %v, concurrent = %v", tl.ReadOnly(in), tl.ConcurrencySafe(in))
	}
	if !searchTool(&fakeBackend{}, true).ReadOnly(in) {
		t.Fatal("ReadOnly config not honoured")
	}
	if got := searchSubject(in); got != "find me" {
		t.Fatalf("subject = %q", got)
	}
}

func TestSearchPromptOnlyWhenEnabled(t *testing.T) {
	system := func(cfg Config) string {
		t.Helper()
		mw, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var out string
		_, err = mw.(agent.ModelWrapper).WrapModel(context.Background(), &agent.ModelRequest{}, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
			out = strings.Join(r.Request.System, "|")
			return &llm.Response{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := system(Config{}); strings.Contains(got, ToolSearch) {
		t.Fatalf("prompt mentions %s without a backend: %q", ToolSearch, got)
	}
	if got := system(Config{Search: &fakeBackend{}}); !strings.Contains(got, ToolSearch) {
		t.Fatalf("prompt lacks %s: %q", ToolSearch, got)
	}
}
