package web

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
)

func TestFetchPermission(t *testing.T) {
	srv := serve(t, "text/plain", "allowed body")
	run := func(cfg Config, rules permission.Rules) (*agent.Final, *llmtest.Scripted) {
		t.Helper()
		mw, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		p := llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "f", Name: ToolFetch, Input: map[string]any{"url": srv.URL + "/doc"}}}},
			llmtest.Turn{Text: "done"},
		)
		a, err := agent.New(agent.Config{Provider: p, Middleware: []agent.Middleware{mw}, Rules: rules})
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
		return final, p
	}
	ranWith := func(p *llmtest.Scripted) bool {
		for _, req := range p.Requests {
			for _, m := range req.Messages {
				for _, b := range m.Content {
					if b.Type == llm.BlockToolResult && len(b.Content) > 0 && strings.Contains(b.Content[0].Text, "allowed body") {
						return true
					}
				}
			}
		}
		return false
	}

	final, _ := run(local, permission.Rules{})
	if final.Reason != agent.ReasonInterrupted {
		t.Fatalf("no rule: reason = %s, want interrupted", final.Reason)
	}

	final, p := run(Config{AllowPrivateNetworks: true, ReadOnly: true}, permission.Rules{})
	if final.Reason != agent.ReasonCompleted || !ranWith(p) {
		t.Fatalf("read-only: reason = %s, ran = %v", final.Reason, ranWith(p))
	}

	rule, err := permission.ParseRule("web_fetch(http://127.0.0.1*)")
	if err != nil {
		t.Fatal(err)
	}
	final, p = run(local, permission.Rules{Allow: []permission.Rule{rule}})
	if final.Reason != agent.ReasonCompleted || !ranWith(p) {
		t.Fatalf("allow rule: reason = %s, ran = %v", final.Reason, ranWith(p))
	}
}

func TestPromptSegment(t *testing.T) {
	custom := "custom web guidance"
	tests := []struct {
		name   string
		prompt *string
		want   string
	}{
		{"default", nil, DefaultPrompt},
		{"custom", &custom, custom},
		{"disabled", new(string), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw, err := New(Config{Prompt: tt.prompt})
			if err != nil {
				t.Fatal(err)
			}
			req := &agent.ModelRequest{Request: llm.Request{System: []string{"host"}}}
			_, err = mw.WrapModel(context.Background(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
				want := []string{"host"}
				if tt.want != "" {
					want = append(want, tt.want)
				}
				if strings.Join(r.Request.System, "|") != strings.Join(want, "|") {
					t.Fatalf("system = %q, want %q", r.Request.System, want)
				}
				return &llm.Response{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// stubSearch is a SearchBackend for the constructor tests.
type stubSearch struct{ err error }

func (stubSearch) Search(context.Context, string, int) ([]SearchResult, error) { return nil, nil }

func (s stubSearch) Validate() error { return s.err }

func TestNewWithSearchOnly(t *testing.T) {
	mw, err := New(Config{DisableFetch: true, Search: stubSearch{}})
	if err != nil {
		t.Fatal(err)
	}
	if mw.Name() != "web" {
		t.Fatalf("name = %q", mw.Name())
	}
	tools := mw.Tools()
	if len(tools) != 1 || tools[0].Name() != ToolSearch {
		t.Fatalf("tools = %v, want only %s", tools, ToolSearch)
	}
}

// TestNewErrors checks the Configs New rejects; a want of "" is a Config
// it must accept.
func TestNewErrors(t *testing.T) {
	notPEM := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := errors.New("missing key")
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"socks proxy", Config{ProxyURL: "socks5://127.0.0.1:1080"}, "proxy"},
		{"proxy without host", Config{ProxyURL: "http://"}, "proxy"},
		{"unparsable proxy", Config{ProxyURL: "http://[::1"}, "proxy"},
		{"missing CA file", Config{CACertFiles: []string{filepath.Join(t.TempDir(), "none.pem")}}, "none.pem"},
		{"non-PEM CA file", Config{CACertFiles: []string{notPEM}}, "no PEM certificates"},
		{"nothing enabled", Config{DisableFetch: true}, "nothing enabled"},
		{"invalid search backend", Config{Search: stubSearch{err: invalid}}, "missing key"},
		{"typed nil search backend", Config{Search: (*Brave)(nil)}, "web: Search is a nil *web.Brave"},
		{"typed nil search, fetch disabled", Config{DisableFetch: true, Search: (*Tavily)(nil)}, "web: Search is a nil *web.Tavily"},
		{"typed nil fetcher", Config{Fetcher: (*HTTPFetcher)(nil)}, "web: Fetcher is a nil *web.HTTPFetcher"},
		{"fetcher with fetch disabled", Config{DisableFetch: true, Search: stubSearch{}, Fetcher: &HTTPFetcher{}}, "web: Fetcher is set but DisableFetch leaves out web_fetch"},
		{"empty CA list with fetcher", Config{Fetcher: &HTTPFetcher{}, CACertFiles: []string{}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

// systemOf returns the system segments mw adds to a request.
func systemOf(t *testing.T, mw *Middleware) string {
	t.Helper()
	var system []string
	req := &agent.ModelRequest{}
	_, err := mw.WrapModel(context.Background(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
		system = r.Request.System
		return &llm.Response{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(system, "\n")
}

func TestPromptDescribesOfferedTools(t *testing.T) {
	searchOnly, err := New(Config{DisableFetch: true, Search: stubSearch{}})
	if err != nil {
		t.Fatal(err)
	}
	got := systemOf(t, searchOnly)
	if strings.Contains(got, "start_index") || strings.Contains(got, "read the web") || strings.Contains(got, "fetched page") {
		t.Errorf("search-only prompt describes web_fetch:\n%s", got)
	}
	if !strings.Contains(got, ToolSearch) || !strings.Contains(got, "untrusted") {
		t.Errorf("search-only prompt lacks web_search or the untrusted-data warning:\n%s", got)
	}

	both, err := New(Config{Search: stubSearch{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := systemOf(t, both); !strings.Contains(got, "start_index") || !strings.Contains(got, ToolSearch) {
		t.Errorf("fetch and search prompt lacks one of them:\n%s", got)
	}
}

func TestZeroHTTPFetcherFails(t *testing.T) {
	u, _ := url.Parse("https://example.com/")
	for name, f := range map[string]*HTTPFetcher{"zero": {}, "nil": nil} {
		if _, err := f.Fetch(context.Background(), u, 10); err == nil || !strings.Contains(err.Error(), "NewHTTPFetcher") {
			t.Errorf("%s HTTPFetcher Fetch = %v, want an error naming NewHTTPFetcher", name, err)
		}
	}
}
