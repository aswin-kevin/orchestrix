package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

func TestCanonicalURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://go.dev/doc", "https://go.dev/doc"},
		{"HTTPS://GO.DEV/Doc", "https://go.dev/Doc"},
		{"https://evil.com", "https://evil.com/"},
		{"https://evil.com?x", "https://evil.com/?x"},
		{"https://evil.com:443/a", "https://evil.com/a"},
		{"http://evil.com:80/a", "http://evil.com/a"},
		{"http://evil.com:443/a", "http://evil.com:443/a"},
		{"https://evil.com:8443/a", "https://evil.com:8443/a"},
		{"https://evil.com./a", "https://evil.com/a"},
		{"https://evil.com../a", "https://evil.com/a"},
		{"https://evil.com/a#frag", "https://evil.com/a"},
		{"https://h/allowed/../secret", "https://h/secret"},
		{"https://h/a/./b/", "https://h/a/b/"},
		{"https://h/a/b/..", "https://h/a/"},
		{"https://h/a/%2e%2E/b", "https://h/b"},
		{"https://h/a%2Fb/c", "https://h/a%2Fb/c"},
		{"https://h//x", "https://h//x"},
		{"http://[::1]:80/", "http://[::1]/"},
		{"http://[FE80::1]:8080/", "http://[fe80::1]:8080/"},
		{"  https://go.dev/x?q=1&r=2  ", "https://go.dev/x?q=1&r=2"},
		// Ports are written in decimal without leading zeros.
		{"https://evil.com:0443/a", "https://evil.com/a"},
		{"http://evil.com:080/a", "http://evil.com/a"},
		{"https://evil.com:00080/a", "https://evil.com:80/a"},
		{"http://[::1]:0080/", "http://[::1]/"},
		// Escaped unreserved characters are decoded; other escapes keep
		// their meaning, with uppercase hex.
		{"https://h/%61dmin/x", "https://h/admin/x"},
		{"https://h/%41dmin/x", "https://h/Admin/x"},
		{"https://h/%7e%2D%2e%5F%31", "https://h/~-._1"},
		{"https://h/a%2fb/%3f", "https://h/a%2Fb/%3F"},
		{"https://h/a/%2e%2e/%2E/b", "https://h/b"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			u, err := canonicalURL(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := u.String(); got != tt.want {
				t.Fatalf("canonicalURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonicalURLErrors(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://x@evil.com/a", "credentials"},
		{"https://go.dev@evil.com/", "credentials"},
		{"https://user:pw@evil.com/", "credentials"},
		{"https://h/../secret", "escapes"},
		{"https://h/a/../../b", "escapes"},
		{"https://./a", "host"},
		{"ftp://h/", "http or https"},
		{"https:evil.com", "http or https"},
		{"https://evil.com:65536/a", "port"},
		{"https://evil.com:0/a", "port"},
		{"https://evil.com:00/a", "port"},
		{"https://evil.com:/a", "port"},
		{"http://[::1]:/a", "port"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			_, err := canonicalURL(tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("canonicalURL(%q) err = %v, want mention of %q", tt.in, err, tt.want)
			}
		})
	}
}

func subjectOf(t *testing.T, tl tool.Tool, u string) string {
	t.Helper()
	raw, err := json.Marshal(FetchInput{URL: u})
	if err != nil {
		t.Fatal(err)
	}
	return tl.(tool.Subjecter).Subject(raw)
}

func rule(t *testing.T, s string) permission.Rule {
	t.Helper()
	r, err := permission.ParseRule(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRulesMatchCanonicalSubject(t *testing.T) {
	tl := fetchTool(t, Config{})
	matches := func(r permission.Rule, u string) bool {
		return r.Matches(permission.Request{Tool: ToolFetch, Subject: subjectOf(t, tl, u)})
	}

	deny := rule(t, "web_fetch(https://evil.com/*)")
	for _, u := range []string{
		"https://evil.com", "https://evil.com?x", "https://evil.com:443/a",
		"https://evil.com./a", "HTTPS://EVIL.COM/a", "https://evil.com/a#f",
		"https://evil.com:0443/a", "https://evil.com:000443/a",
	} {
		if !matches(deny, u) {
			t.Errorf("deny rule misses %q (subject %q)", u, subjectOf(t, tl, u))
		}
	}
	// URLs with credentials are refused before any request, whatever the
	// rules say. All traffic goes to a local proxy without DNS, so even a
	// regression cannot reach the network.
	px, seen := proxy(t)
	offline := fetchTool(t, Config{ProxyURL: px.URL, AllowPrivateNetworks: true})
	res := runFetch(t, offline, FetchInput{URL: "http://x@evil.test/a"})
	if !res.IsError || !strings.Contains(res.Flatten(), "credentials") {
		t.Errorf("userinfo URL: %+v", res)
	}
	if got := seen.Load(); got != nil {
		t.Errorf("request sent for a URL with credentials: %v", got)
	}

	admin := rule(t, "web_fetch(https://evil.com/admin/*)")
	if !matches(admin, "https://evil.com/%61dmin/x") {
		t.Errorf("deny rule misses /%%61dmin/x (subject %q)", subjectOf(t, tl, "https://evil.com/%61dmin/x"))
	}
	// %41 is "A": the server sees /Admin, a different path from /admin, so
	// the rule rightly does not match it.
	if got := subjectOf(t, tl, "https://evil.com/%41dmin/x"); got != "https://evil.com/Admin/x" || matches(admin, "https://evil.com/%41dmin/x") {
		t.Errorf("/%%41dmin/x: subject %q, matched %v", got, matches(admin, "https://evil.com/%41dmin/x"))
	}

	if matches(rule(t, "web_fetch(https://go.dev*)"), "https://go.dev@evil.com/") {
		t.Error("allow https://go.dev* matches a URL whose host is evil.com")
	}
	if matches(rule(t, "web_fetch(https://h/allowed/*)"), "https://h/allowed/../secret") {
		t.Error("allow https://h/allowed/* matches a path that leaves /allowed")
	}
}

func TestRequestUsesCanonicalURL(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	in := strings.Replace(srv.URL, "http://", "HTTP://", 1) + "/a/../b/./c?q=1#frag"
	got := runFetch(t, fetchTool(t, local), FetchInput{URL: in}).Flatten()
	if gotPath != "/b/c" || gotQuery != "q=1" {
		t.Fatalf("server saw path %q query %q", gotPath, gotQuery)
	}
	if !strings.HasPrefix(got, "URL: "+srv.URL+"/b/c?q=1\n") {
		t.Fatalf("output = %q", got)
	}
}
