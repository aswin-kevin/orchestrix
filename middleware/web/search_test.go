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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
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
	b := must(NewDuckDuckGo(DuckDuckGoConfig{BaseURL: srv.URL, UserAgent: "test-agent"}))
	res, err := b.Search(context.Background(), "golang & more", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []SearchResult{
		{"The Go Programming Language", "https://go.dev/doc?a=1&b=2", "Go is an open source language."},
		{"Plain protocol-relative", "https://example.org/plain", "Second snippet"},
		{"Direct", "https://example.net/direct", ""},
	}
	if !reflect.DeepEqual(res, want) {
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
	res, err := must(NewDuckDuckGo(DuckDuckGoConfig{BaseURL: srv.URL})).Search(context.Background(), "q", 2)
	if err != nil || len(res) != 2 {
		t.Fatalf("results = %v, err = %v", res, err)
	}
}

func TestBraveRequestAndParse(t *testing.T) {
	srv, got := backendServer(t, 200, "application/json", `{"web":{"results":[
		{"title":" One ","url":"https://one.example/","description":" first "},
		{"title":"Bad","url":"ftp://bad.example/","description":"dropped"},
		{"title":"Two","url":"https://two.example/","description":"second"}]}}`)
	b := must(NewBrave(BraveConfig{APIKey: "k", BaseURL: srv.URL}))
	res, err := b.Search(context.Background(), "go lang", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []SearchResult{{"One", "https://one.example/", "first"}, {"Two", "https://two.example/", "second"}}
	if !reflect.DeepEqual(res, want) {
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
	b := must(NewTavily(TavilyConfig{APIKey: "secret", BaseURL: srv.URL}))
	res, err := b.Search(context.Background(), "query", 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := []SearchResult{{"A", "https://a.example/", "about a"}}; !reflect.DeepEqual(res, want) {
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

// backends builds each backend against a test endpoint.
var backends = map[string]func(url string) (SearchBackend, error){
	"duckduckgo": func(u string) (SearchBackend, error) { return NewDuckDuckGo(DuckDuckGoConfig{BaseURL: u}) },
	"brave":      func(u string) (SearchBackend, error) { return NewBrave(BraveConfig{APIKey: "k", BaseURL: u}) },
	"tavily":     func(u string) (SearchBackend, error) { return NewTavily(TavilyConfig{APIKey: "k", BaseURL: u}) },
}

func TestSearchHTTPErrorStatus(t *testing.T) {
	long := strings.Repeat("x", 1000)
	for name, mk := range backends {
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
	pad := strings.Repeat("y", maxSearchBody)
	bodies := map[string]string{
		"duckduckgo": `<div class="result"><a class="result__a" href="https://a.example/">A</a></div><!--` + pad + `-->`,
		"brave":      `{"web":{"results":[{"title":"A","url":"https://a.example/","description":"` + pad + `"}]}}`,
		"tavily":     `{"results":[{"title":"A","url":"https://a.example/","content":"` + pad + `"}]}`,
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			srv, _ := backendServer(t, 200, "text/plain", bodies[name])
			b, err := mk(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = b.Search(context.Background(), "q", 5)
			if err == nil || err.Error() != name+": response exceeds 2 MiB" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestSearchDoesNotFollowRedirects(t *testing.T) {
	second, hit := backendServer(t, 200, "application/json", `{}`)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusFound)
	}))
	defer first.Close()
	_, err := must(NewBrave(BraveConfig{APIKey: "k", BaseURL: first.URL})).Search(context.Background(), "q", 3)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("err = %v", err)
	}
	if hit.method != "" {
		t.Fatalf("redirect target was requested (%s)", hit.method)
	}
}

func TestDuckDuckGoDropsEmptyLinks(t *testing.T) {
	srv, _ := backendServer(t, 200, "text/html", `
<div class="result"><a class="result__a" href="">Empty</a></div>
<div class="result"><a class="result__a" href="//duckduckgo.com/l/?uddg=">Empty uddg</a></div>
<div class="result"><a class="result__a" href="//duckduckgo.com/l/">Bare</a></div>
<div class="result"><a class="result__a">No href</a></div>`)
	res, err := must(NewDuckDuckGo(DuckDuckGoConfig{BaseURL: srv.URL})).Search(context.Background(), "q", 5)
	if err != nil || len(res) != 0 {
		t.Fatalf("results = %v, err = %v", res, err)
	}
}

func TestDuckDuckGoChallengeIsAnError(t *testing.T) {
	srv, _ := backendServer(t, 202, "text/html", "<html>verify you are human</html>")
	_, err := must(NewDuckDuckGo(DuckDuckGoConfig{BaseURL: srv.URL})).Search(context.Background(), "q", 5)
	if err == nil || err.Error() != "duckduckgo: HTTP 202 (challenge)" {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchNonPositiveN(t *testing.T) {
	srv, _ := backendServer(t, 200, "application/json", `{"results":[{"title":"A","url":"https://a.example/"}]}`)
	for _, n := range []int{0, -1} {
		res, err := must(NewTavily(TavilyConfig{APIKey: "k", BaseURL: srv.URL})).Search(context.Background(), "q", n)
		if err != nil || res != nil {
			t.Fatalf("n = %d: results = %v, err = %v", n, res, err)
		}
	}
	if clean([]SearchResult{{"A", "https://a.example/", ""}}, 0) != nil {
		t.Fatal("clean with n = 0 returned results")
	}
}

func TestBraveBaseURLWithQuery(t *testing.T) {
	srv, got := backendServer(t, 200, "application/json", `{"web":{"results":[]}}`)
	b := must(NewBrave(BraveConfig{APIKey: "k", BaseURL: srv.URL + "/search?country=us"}))
	if _, err := b.Search(context.Background(), "go", 2); err != nil {
		t.Fatal(err)
	}
	if got.query.Get("country") != "us" || got.query.Get("q") != "go" || got.query.Get("count") != "2" {
		t.Fatalf("query = %v", got.query)
	}
}

func TestBraveInvalidJSON(t *testing.T) {
	srv, _ := backendServer(t, 200, "text/html", "<html>not json</html>")
	_, err := must(NewBrave(BraveConfig{APIKey: "k", BaseURL: srv.URL})).Search(context.Background(), "q", 3)
	if err == nil || !strings.Contains(err.Error(), "invalid response") {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchCancelledContext(t *testing.T) {
	srv, _ := backendServer(t, 200, "application/json", `{}`)
	mw, err := New(Config{DisableFetch: true, Search: must(NewBrave(BraveConfig{APIKey: "k", BaseURL: srv.URL}))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := mw.Tools()[0].Call(ctx, json.RawMessage(`{"query":"q"}`), &tool.Call{})
	if err != nil || !res.IsError {
		t.Fatalf("result = %q (error %v), err = %v", res.Flatten(), res.IsError, err)
	}
}

func TestSearchToolAbsentWithoutBackend(t *testing.T) {
	mw, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range mw.Tools() {
		if tl.Name() == ToolSearch {
			t.Fatal("web_search offered without a backend")
		}
	}
}

func TestSearchPermission(t *testing.T) {
	run := func(cfg Config) (agent.TerminalReason, string) {
		t.Helper()
		cfg.DisableFetch, cfg.Search = true, &fakeBackend{results: []SearchResult{{"Hit", "https://hit.example/", "s"}}}
		mw, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		p := llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s", Name: ToolSearch, Input: map[string]any{"query": "q"}}}},
			llmtest.Turn{Text: "done"},
		)
		a, err := agent.New(agent.Config{Provider: p, Middleware: []agent.Middleware{mw}})
		if err != nil {
			t.Fatal(err)
		}
		var final *agent.Final
		for ev, err := range a.Run(context.Background(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}) {
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind == agent.KindFinal {
				final = ev.Final
			}
		}
		var out strings.Builder
		for _, req := range p.Requests {
			for _, m := range req.Messages {
				for _, b := range m.Content {
					if b.Type == llm.BlockToolResult && len(b.Content) > 0 {
						out.WriteString(b.Content[0].Text)
					}
				}
			}
		}
		return final.Reason, out.String()
	}
	if reason, out := run(Config{}); reason != agent.ReasonInterrupted || strings.Contains(out, "hit.example") {
		t.Fatalf("no rule: reason = %s, output = %q", reason, out)
	}
	if reason, out := run(Config{ReadOnly: true}); reason != agent.ReasonCompleted || !strings.Contains(out, "hit.example") {
		t.Fatalf("read-only: reason = %s, output = %q", reason, out)
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
	b := must(NewTavily(TavilyConfig{APIKey: "k", BaseURL: "http://search.invalid/search", Client: client}))
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

func TestErrNoAPIKeyPrefix(t *testing.T) {
	if !strings.HasPrefix(ErrNoAPIKey.Error(), "web: ") {
		t.Fatalf("ErrNoAPIKey = %q, want the web: prefix", ErrNoAPIKey)
	}
	_, braveErr := NewBrave(BraveConfig{})
	_, tavilyErr := NewTavily(TavilyConfig{})
	for name, err := range map[string]error{"brave": braveErr, "tavily": tavilyErr} {
		if !errors.Is(err, ErrNoAPIKey) || !strings.HasPrefix(err.Error(), "web: ") {
			t.Fatalf("%s err = %v, want ErrNoAPIKey with the web: prefix", name, err)
		}
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
		res, err := mw.Tools()[0].Call(context.Background(), json.RawMessage(input), &tool.Call{})
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
	if isReadOnly(tl, in) || !isConcurrent(tl, in) {
		t.Fatalf("readOnly = %v, concurrent = %v", isReadOnly(tl, in), isConcurrent(tl, in))
	}
	if !isReadOnly(searchTool(&fakeBackend{}, true), in) {
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
		_, err = mw.WrapModel(context.Background(), &agent.ModelRequest{}, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
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
