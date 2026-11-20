package web

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/tool"
)

// fetchTool returns the web_fetch tool of a middleware built from cfg.
func fetchTool(t *testing.T, cfg Config) tool.Tool {
	t.Helper()
	mw, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range mw.(agent.ToolProvider).Tools() {
		if tl.Name() == ToolFetch {
			return tl
		}
	}
	t.Fatal("no web_fetch tool")
	return nil
}

func runFetch(t *testing.T, tl tool.Tool, in FetchInput) tool.Result {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.Call(context.Background(), raw, &tool.Call{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// serve starts an httptest server answering every request with body.
func serve(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// local is the config of tests that fetch from loopback httptest servers.
var local = Config{AllowPrivateNetworks: true}

func TestFetchHTMLConverted(t *testing.T) {
	var gotUA, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotAccept = r.UserAgent(), r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Docs Home</title></head><body><h1>Welcome</h1><p>See <a href="/guide">the guide</a>.</p></body></html>`)
	}))
	defer srv.Close()

	res := runFetch(t, fetchTool(t, local), FetchInput{URL: srv.URL + "/start"})
	if res.IsError {
		t.Fatalf("error result: %s", res.Flatten())
	}
	got := res.Flatten()
	want := "URL: " + srv.URL + "/start\nTitle: Docs Home\n\n# Welcome\n\nSee [the guide](" + srv.URL + "/guide)."
	if got != want {
		t.Fatalf("output =\n%q\nwant\n%q", got, want)
	}
	if gotUA != DefaultUserAgent {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	if gotAccept != acceptHeader {
		t.Fatalf("Accept = %q", gotAccept)
	}
}

func TestFetchCustomUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.UserAgent()
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	runFetch(t, fetchTool(t, Config{AllowPrivateNetworks: true, UserAgent: "probe/1"}), FetchInput{URL: srv.URL})
	if gotUA != "probe/1" {
		t.Fatalf("User-Agent = %q", gotUA)
	}
}

func TestFetchPlainAndJSON(t *testing.T) {
	tests := []struct{ ct, body string }{
		{"text/plain; charset=utf-8", "plain text\nline two"},
		{"application/json", `{"a": 1}`},
		{"application/problem+json", `{"title": "x"}`},
		{"application/xml", `<a>1</a>`},
		{"text/csv", "a,b\n1,2"},
	}
	for _, tt := range tests {
		t.Run(tt.ct, func(t *testing.T) {
			srv := serve(t, tt.ct, tt.body)
			res := runFetch(t, fetchTool(t, local), FetchInput{URL: srv.URL})
			want := "URL: " + srv.URL + "/\nTitle: \n\n" + tt.body
			if res.IsError || res.Flatten() != want {
				t.Fatalf("result = %+v, want %q", res, want)
			}
		})
	}
}

func TestFetchInvalidUTF8Replaced(t *testing.T) {
	srv := serve(t, "text/plain", "ok\xff\xfeend")
	got := runFetch(t, fetchTool(t, local), FetchInput{URL: srv.URL}).Flatten()
	if !strings.HasSuffix(got, "ok�end") {
		t.Fatalf("output = %q", got)
	}
}

func TestUnsupportedContentType(t *testing.T) {
	srv := serve(t, "image/png", "\x89PNG\r\n\x1a\n")
	res := runFetch(t, fetchTool(t, local), FetchInput{URL: srv.URL})
	if !res.IsError || !strings.Contains(res.Flatten(), "unsupported content type image/png") {
		t.Fatalf("result = %+v", res)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<p>No such page</p>"+strings.Repeat("x", 3000))
	}))
	defer srv.Close()
	res := runFetch(t, fetchTool(t, local), FetchInput{URL: srv.URL + "/missing"})
	got := res.Flatten()
	if !res.IsError || !strings.HasPrefix(got, "HTTP 404 for "+srv.URL+"/missing") || !strings.Contains(got, "No such page") {
		t.Fatalf("result = %+v", res)
	}
	if len(got) > 2200 {
		t.Fatalf("error body not cut: %d bytes", len(got))
	}
}

func TestFetchPaging(t *testing.T) {
	body := strings.Repeat("é", 120) // multi-byte runes: paging is by character
	srv := serve(t, "text/plain", body)
	tl := fetchTool(t, local)
	header := "URL: " + srv.URL + "/\nTitle: \n\n"

	first := runFetch(t, tl, FetchInput{URL: srv.URL, MaxChars: 50}).Flatten()
	note := "\n\n[Content truncated: characters 0–50 of 120. Call web_fetch with start_index=50 to continue.]"
	if want := header + strings.Repeat("é", 50) + note; first != want {
		t.Fatalf("first page =\n%q\nwant\n%q", first, want)
	}
	second := runFetch(t, tl, FetchInput{URL: srv.URL, StartIndex: 50, MaxChars: 50}).Flatten()
	if !strings.Contains(second, "characters 50–100 of 120") || !strings.Contains(second, "start_index=100") {
		t.Fatalf("second page = %q", second)
	}
	last := runFetch(t, tl, FetchInput{URL: srv.URL, StartIndex: 100, MaxChars: 50}).Flatten()
	if want := header + strings.Repeat("é", 20); last != want {
		t.Fatalf("last page = %q", last)
	}
	past := runFetch(t, tl, FetchInput{URL: srv.URL, StartIndex: 500})
	if !past.IsError || !strings.Contains(past.Flatten(), "start_index") {
		t.Fatalf("past the end = %+v", past)
	}
}

func TestFetchMaxCharsCappedByConfig(t *testing.T) {
	srv := serve(t, "text/plain", strings.Repeat("a", 100))
	got := runFetch(t, fetchTool(t, Config{AllowPrivateNetworks: true, MaxChars: 10}), FetchInput{URL: srv.URL, MaxChars: 1000}).Flatten()
	if !strings.Contains(got, "characters 0–10 of 100") {
		t.Fatalf("output = %q", got)
	}
}

func TestFetchSizeCap(t *testing.T) {
	const total, maxBytes = 10 << 20, 1 << 20
	stopped := make(chan bool, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		chunk := []byte(strings.Repeat("a", 64<<10))
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				stopped <- true
				return
			}
		}
		stopped <- false
	}))
	defer srv.Close()

	got := runFetch(t, fetchTool(t, Config{AllowPrivateNetworks: true, MaxBytes: maxBytes, MaxChars: 10}), FetchInput{URL: srv.URL}).Flatten()
	if !strings.Contains(got, fmt.Sprintf("of %d.", maxBytes+len("\n\n[response truncated at 1048576 bytes]"))) {
		t.Fatalf("output = %q", got)
	}
	tail := runFetch(t, fetchTool(t, Config{AllowPrivateNetworks: true, MaxBytes: maxBytes}), FetchInput{URL: srv.URL, StartIndex: maxBytes}).Flatten()
	if !strings.Contains(tail, "[response truncated at 1048576 bytes]") {
		t.Fatalf("tail = %q", tail)
	}
	for range 2 {
		select {
		case s := <-stopped:
			if !s {
				t.Fatal("handler wrote the whole body: the client kept reading")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("handler still writing: the client kept reading")
		}
	}
}

func TestFetchTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	start := time.Now()
	res := runFetch(t, fetchTool(t, Config{AllowPrivateNetworks: true, Timeout: 200 * time.Millisecond}), FetchInput{URL: srv.URL})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s", elapsed)
	}
	if !res.IsError || !strings.Contains(res.Flatten(), "timed out") {
		t.Fatalf("result = %+v", res)
	}
}

func TestRedirectFollowed(t *testing.T) {
	target := serve(t, "text/plain", "landed")
	start := httptest.NewServer(http.RedirectHandler(target.URL+"/final", http.StatusMovedPermanently))
	defer start.Close()
	got := runFetch(t, fetchTool(t, local), FetchInput{URL: start.URL}).Flatten()
	if !strings.HasPrefix(got, "URL: "+target.URL+"/final\n") || !strings.HasSuffix(got, "landed") {
		t.Fatalf("output = %q", got)
	}
}

func TestRedirectLimit(t *testing.T) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
	}))
	defer srv.Close()
	res := runFetch(t, fetchTool(t, local), FetchInput{URL: srv.URL})
	if !res.IsError || !strings.Contains(res.Flatten(), "too many redirects") {
		t.Fatalf("result = %+v", res)
	}
	if n := hits.Load(); n != DefaultMaxRedirects+1 {
		t.Fatalf("server hit %d times, want %d", n, DefaultMaxRedirects+1)
	}
}

func TestRedirectToOtherSchemeRefused(t *testing.T) {
	srv := httptest.NewServer(http.RedirectHandler("ftp://example.test/file", http.StatusFound))
	defer srv.Close()
	res := runFetch(t, fetchTool(t, local), FetchInput{URL: srv.URL})
	if !res.IsError || !strings.Contains(res.Flatten(), "scheme") {
		t.Fatalf("result = %+v", res)
	}
}

func TestSchemeRejected(t *testing.T) {
	tl := fetchTool(t, Config{})
	for _, u := range []string{"file:///etc/passwd", "ftp://x", "javascript:alert(1)", "http://", "not a url", "//host/path"} {
		res := runFetch(t, tl, FetchInput{URL: u})
		if !res.IsError || !strings.Contains(res.Flatten(), "http or https") {
			t.Fatalf("%q: result = %+v", u, res)
		}
	}
}

func writePEM(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCACertFile(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "secure")
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the untrusted handshake is expected
	srv.StartTLS()
	defer srv.Close()

	trusted := fetchTool(t, Config{AllowPrivateNetworks: true, CACertFiles: []string{writePEM(t, srv)}})
	if res := runFetch(t, trusted, FetchInput{URL: srv.URL}); res.IsError || !strings.HasSuffix(res.Flatten(), "secure") {
		t.Fatalf("with CA file: %+v", res)
	}
	res := runFetch(t, fetchTool(t, local), FetchInput{URL: srv.URL})
	if !res.IsError || !strings.Contains(res.Flatten(), "certificate") {
		t.Fatalf("without CA file: %+v", res)
	}
}

func TestFetchToolTraits(t *testing.T) {
	raw := json.RawMessage(`{"url":"https://go.dev/doc"}`)
	tl := fetchTool(t, Config{})
	if !tl.ConcurrencySafe(raw) || tl.ReadOnly(raw) {
		t.Fatalf("concurrent = %v, read-only = %v", tl.ConcurrencySafe(raw), tl.ReadOnly(raw))
	}
	if got := tl.(tool.Subjecter).Subject(raw); got != "https://go.dev/doc" {
		t.Fatalf("subject = %q", got)
	}
	if !fetchTool(t, Config{ReadOnly: true}).ReadOnly(raw) {
		t.Fatal("ReadOnly config not reported")
	}
}

func TestFetchTimeoutCoversConversion(t *testing.T) {
	srv := serve(t, "text/html", "<pre>"+strings.Repeat("`", 585<<10)+"</pre>")
	tl := fetchTool(t, Config{AllowPrivateNetworks: true, Timeout: 2 * time.Second})
	done := make(chan tool.Result, 1)
	start := time.Now()
	go func() { done <- runFetch(t, tl, FetchInput{URL: srv.URL}) }()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("fetch took %s with a 2s timeout", elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("fetch still running 30s past a 2s timeout")
	}
}

func TestFetchMarkdownTruncatedNote(t *testing.T) {
	// Each short paragraph gains a line of quote markers, so the markdown
	// outgrows the body: the body is read whole, the markdown is cut.
	const maxBytes = 4096
	doc := strings.Repeat("<blockquote>", maxNesting) + strings.Repeat("<p>a</p>", 400)
	if len(doc) > maxBytes {
		t.Fatalf("document is %d bytes, want at most %d", len(doc), maxBytes)
	}
	srv := serve(t, "text/html", doc)
	got := runFetch(t, fetchTool(t, Config{AllowPrivateNetworks: true, MaxBytes: maxBytes}), FetchInput{URL: srv.URL}).Flatten()
	if !strings.HasSuffix(got, "\n\n[content truncated]") {
		t.Fatalf("output = %q, want the truncation note", got)
	}
}

// roundTripFunc is an http.RoundTripper calling itself.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// slowClose is a body whose Close waits for d after it has been read.
type slowClose struct {
	io.Reader
	d time.Duration
}

func (b slowClose) Close() error { time.Sleep(b.d); return nil }

func TestFetchFinishedContentOutlivesDeadline(t *testing.T) {
	// The body is read whole before the deadline, which passes before
	// conversion starts: a body that needs no conversion is returned, an
	// HTML conversion cut short by the deadline is a timeout.
	for ct, wantErr := range map[string]bool{"text/plain": false, "application/json": false, "text/html": true} {
		t.Run(ct, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {ct}},
					Body:       slowClose{strings.NewReader(`"done"`), 100 * time.Millisecond},
					Request:    r,
				}, nil
			})}
			f := newFetcher(client, Config{Timeout: 20 * time.Millisecond})
			res, err := f.fetch(context.Background(), FetchInput{URL: "https://site.test/"}, &tool.Call{})
			if err != nil {
				t.Fatal(err)
			}
			got := res.Flatten()
			if wantErr {
				if !res.IsError || !strings.Contains(got, "timed out") {
					t.Fatalf("result = %q, want a timeout", got)
				}
				return
			}
			if res.IsError || !strings.Contains(got, `"done"`) {
				t.Fatalf("result = %q, want the content", got)
			}
		})
	}
}
