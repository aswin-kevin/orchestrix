package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFetcher is a Fetcher that returns page and err and records the
// request it got.
type fakeFetcher struct {
	page     *Page
	err      error
	url      string
	maxBytes int64
}

func (f *fakeFetcher) Fetch(_ context.Context, u *url.URL, maxBytes int64) (*Page, error) {
	f.url, f.maxBytes = u.String(), maxBytes
	return f.page, f.err
}

func TestName(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if Name != "web" || m.Name() != Name {
		t.Fatalf("Name = %q, m.Name() = %q", Name, m.Name())
	}
}

func TestCustomFetcher(t *testing.T) {
	fake := &fakeFetcher{page: &Page{URL: "https://x/y", Title: "T", Text: "body"}}
	res := runFetch(t, fetchTool(t, Config{Fetcher: fake}), FetchInput{URL: "https://x/start"})
	if got, want := res.Flatten(), "URL: https://x/y\nTitle: T\n\nbody"; res.IsError || !strings.HasPrefix(got, want) {
		t.Fatalf("result = %q, want prefix %q", got, want)
	}
	if fake.maxBytes != DefaultMaxBytes {
		t.Fatalf("maxBytes = %d, want %d", fake.maxBytes, DefaultMaxBytes)
	}
	if fake.url != "https://x/start" {
		t.Fatalf("url = %q", fake.url)
	}
}

func TestCustomFetcherTruncatedAndPaged(t *testing.T) {
	fake := &fakeFetcher{page: &Page{Text: strings.Repeat("a", 30), Truncated: true}}
	got := runFetch(t, fetchTool(t, Config{Fetcher: fake, MaxBytes: 30, MaxChars: 10}), FetchInput{URL: "https://x/"}).Flatten()
	// An empty Page.URL falls back to the URL requested.
	if !strings.HasPrefix(got, "URL: https://x/\nTitle: \n\n") || !strings.Contains(got, "start_index=10") {
		t.Fatalf("result = %q", got)
	}
	tail := runFetch(t, fetchTool(t, Config{Fetcher: fake, MaxBytes: 30}), FetchInput{URL: "https://x/"}).Flatten()
	if !strings.HasSuffix(tail, "\n\n[response truncated at 30 bytes]") {
		t.Fatalf("result = %q, want the response truncation note", tail)
	}
}

func TestCustomFetcherStatusError(t *testing.T) {
	fake := &fakeFetcher{page: &Page{URL: "https://x/y", Status: 404, Text: "gone"}}
	res := runFetch(t, fetchTool(t, Config{Fetcher: fake}), FetchInput{URL: "https://x/y"})
	if got := res.Flatten(); !res.IsError || !strings.Contains(got, "HTTP 404") || !strings.Contains(got, "gone") {
		t.Fatalf("result = %+v", res)
	}
}

func TestCustomFetcherError(t *testing.T) {
	fake := &fakeFetcher{err: errors.New("no route")}
	res := runFetch(t, fetchTool(t, Config{Fetcher: fake}), FetchInput{URL: "https://x/y"})
	if got := res.Flatten(); !res.IsError || got != "fetching https://x/y failed: no route" {
		t.Fatalf("result = %+v", res)
	}
	nilPage := runFetch(t, fetchTool(t, Config{Fetcher: &fakeFetcher{}}), FetchInput{URL: "https://x/y"})
	if !nilPage.IsError {
		t.Fatalf("nil page: result = %+v, want an error", nilPage)
	}
}

func TestCustomFetcherTimeout(t *testing.T) {
	var deadline atomic.Bool
	f := fetcherFunc(func(ctx context.Context, _ *url.URL, _ int64) (*Page, error) {
		_, ok := ctx.Deadline()
		deadline.Store(ok)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	res := runFetch(t, fetchTool(t, Config{Fetcher: f, Timeout: 50 * time.Millisecond}), FetchInput{URL: "https://x/"})
	if !deadline.Load() || !res.IsError || !strings.Contains(res.Flatten(), "timed out") {
		t.Fatalf("deadline = %v, result = %+v", deadline.Load(), res)
	}
}

// fetcherFunc is a Fetcher calling itself.
type fetcherFunc func(context.Context, *url.URL, int64) (*Page, error)

func (f fetcherFunc) Fetch(ctx context.Context, u *url.URL, maxBytes int64) (*Page, error) {
	return f(ctx, u, maxBytes)
}

func TestFetcherExcludesClientFields(t *testing.T) {
	fake := &fakeFetcher{}
	for name, cfg := range map[string]Config{
		"MaxRedirects":         {Fetcher: fake, MaxRedirects: 3},
		"AllowPrivateNetworks": {Fetcher: fake, AllowPrivateNetworks: true},
		"ProxyURL":             {Fetcher: fake, ProxyURL: "http://proxy:3128"},
		"CACertFiles":          {Fetcher: fake, CACertFiles: []string{"ca.pem"}},
		"UserAgent":            {Fetcher: fake, UserAgent: "probe/1"},
	} {
		if _, err := New(cfg); err == nil || err.Error() != "web: Fetcher excludes the HTTP client settings" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Timeout, MaxBytes and MaxChars are the middleware's own.
	if _, err := New(Config{Fetcher: fake, Timeout: time.Second, MaxBytes: 10, MaxChars: 10}); err != nil {
		t.Fatal(err)
	}
}

func TestNegativeLimitsRejected(t *testing.T) {
	for name, cfg := range map[string]Config{
		"MaxBytes":     {MaxBytes: -1},
		"MaxChars":     {MaxChars: -1},
		"Timeout":      {Timeout: -time.Second},
		"MaxRedirects": {MaxRedirects: -1},
	} {
		_, err := New(cfg)
		if err == nil || !strings.HasPrefix(err.Error(), "web: ") || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v, want a web: error naming the field", name, err)
		}
	}
	for name, cc := range map[string]ClientConfig{
		"Timeout":      {Timeout: -time.Second},
		"MaxRedirects": {MaxRedirects: -1},
	} {
		if _, err := NewHTTPClient(cc); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("NewHTTPClient %s: err = %v", name, err)
		}
		if _, err := NewHTTPFetcher(cc); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("NewHTTPFetcher %s: err = %v", name, err)
		}
	}
}

func TestHTTPFetcherBlocksPrivate(t *testing.T) {
	var hits atomic.Int32
	srv := serveCounting(t, &hits)
	f, err := NewHTTPFetcher(ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL)
	_, err = f.Fetch(t.Context(), u, DefaultMaxBytes)
	if !errors.Is(err, ErrBlockedAddress) || !strings.Contains(err.Error(), u.Hostname()) {
		t.Fatalf("err = %v, want ErrBlockedAddress naming %s", err, u.Hostname())
	}
	res := runFetch(t, fetchTool(t, Config{}), FetchInput{URL: srv.URL})
	if !res.IsError || !strings.Contains(res.Flatten(), u.Hostname()) || !strings.Contains(res.Flatten(), "blocked") {
		t.Fatalf("result = %+v, want the blocked address named", res)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("handler hit %d times", n)
	}
}

func TestHTTPFetcherPage(t *testing.T) {
	srv := serve(t, "text/html", "<title>Hi</title><p>text</p>")
	f, err := NewHTTPFetcher(ClientConfig{AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL + "/p")
	page, err := f.Fetch(t.Context(), u, DefaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	want := Page{URL: srv.URL + "/p", Status: 200, Title: "Hi", Text: "text"}
	if *page != want {
		t.Fatalf("page = %+v, want %+v", *page, want)
	}
}

// serveCounting starts an httptest server that counts its requests.
func serveCounting(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(counting(hits, "text/plain", "secret"))
	t.Cleanup(srv.Close)
	return srv
}

// newFetcher returns the web_fetch runner of cfg over an HTTPFetcher with
// client, for tests that need a client New does not build.
func newFetcher(client *http.Client, cfg Config) *fetcher {
	return newFetchTool(&HTTPFetcher{client: client}, cfg)
}
