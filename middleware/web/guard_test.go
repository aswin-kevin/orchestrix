package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBlockedAddresses(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"fe80::1", true},
		{"fc00::1", true},
		{"100.64.0.1", true},
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"::", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"224.0.0.1", true},
		{"ff02::1", true},
		{"192.0.0.8", true},
		{"198.18.0.1", true},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		// IPv4 embedded in NAT64 and 6to4 addresses.
		{"64:ff9b::7f00:1", true},
		{"64:ff9b::a9fe:a9fe", true},
		{"2002:7f00:1::", true},
		{"2002:a9fe:a9fe::", true},
		{"2002:a00:1::1", true},
		// Local-use NAT64, IPv4-compatible, site-local and Teredo.
		{"64:ff9b:1::5db8:d822", true},
		{"64:ff9b:1:ffff::1", true},
		{"::7f00:1", true},
		{"::a9fe:a9fe", true},
		{"fec0::1", true},
		{"2001::1", true},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", true},
		// Public addresses.
		{"93.184.216.34", false},
		{"2606:4700::1111", false},
		{"64:ff9b::5db8:d822", false},
		{"2002:5db8:d822::1", false},
		{"172.32.0.1", false},
		{"100.128.0.1", false},
		{"::5db8:d822", false},
		{"2001:db9::1", false},
		{"2001:1::1", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			if got := blockedIP(netip.MustParseAddr(tt.ip)); got != tt.want {
				t.Fatalf("blockedIP(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestPublicOnlyPolicy(t *testing.T) {
	if err := publicOnly("tcp", "93.184.216.34:443"); err != nil {
		t.Fatalf("public address refused: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:80", "[::ffff:10.0.0.1]:80", "[fe80::1%en0]:80", "not-an-address"} {
		if err := publicOnly("tcp", addr); err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Fatalf("publicOnly(%q) = %v, want blocked", addr, err)
		}
	}
}

// counting returns a handler that counts its hits and answers with body.
func counting(hits *atomic.Int32, contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", contentType)
		fmt.Fprint(w, body)
	}
}

func TestFetchLoopbackBlockedByDefault(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(counting(&hits, "text/plain", "secret"))
	defer srv.Close()
	port := srv.URL[strings.LastIndexByte(srv.URL, ':')+1:]

	tl := fetchTool(t, Config{})
	for _, u := range []string{srv.URL, "http://localhost:" + port + "/", "http://app.localhost:" + port + "/"} {
		res := runFetch(t, tl, FetchInput{URL: u})
		if !res.IsError || !strings.Contains(res.Flatten(), "blocked") {
			t.Fatalf("%s: result = %+v, want blocked error", u, res)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("handler hit %d times", n)
	}
}

func TestRedirectToBlockedAddressRefused(t *testing.T) {
	var hits atomic.Int32
	second := httptest.NewServer(counting(&hits, "text/plain", "secret"))
	defer second.Close()
	first := httptest.NewServer(http.RedirectHandler(second.URL+"/meta", http.StatusFound))
	defer first.Close()

	allowed := strings.TrimPrefix(first.URL, "http://")
	policy := func(_, address string) error {
		if address == allowed {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
	}
	client, err := newClient(ClientConfig{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	f := newFetcher(client, Config{})
	res, err := f.fetch(context.Background(), FetchInput{URL: first.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Flatten(), "blocked") {
		t.Fatalf("result = %+v, want blocked error", res)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("redirect target hit %d times", n)
	}
}

// proxy is an httptest proxy that records the request target of every
// request it receives and answers each one itself.
func proxy(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.RequestURI)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "via proxy")
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestProxyUsed(t *testing.T) {
	px, seen := proxy(t)
	// A public literal target needs no DNS, and the proxy answers itself, so
	// nothing leaves the machine. The proxy is on loopback, which the
	// default policy allows only because it is the configured proxy.
	tl := fetchTool(t, Config{ProxyURL: px.URL})
	res := runFetch(t, tl, FetchInput{URL: "http://93.184.216.34/page"})
	if res.IsError || !strings.Contains(res.Flatten(), "via proxy") {
		t.Fatalf("result = %+v", res)
	}
	if got, _ := seen.Load().(string); got != "http://93.184.216.34/page" {
		t.Fatalf("proxy saw %q, want the absolute-form target", got)
	}
}

func TestProxyTargetStillChecked(t *testing.T) {
	px, seen := proxy(t)
	tl := fetchTool(t, Config{ProxyURL: px.URL})
	for _, u := range []string{"http://127.0.0.1:9/", "http://[::1]:9/", "http://localhost/"} {
		res := runFetch(t, tl, FetchInput{URL: u})
		if !res.IsError || !strings.Contains(res.Flatten(), "blocked") {
			t.Fatalf("%s: result = %+v, want blocked error", u, res)
		}
	}
	if got := seen.Load(); got != nil {
		t.Fatalf("proxy received %v", got)
	}
}

func TestProxyRedirectToBlockedAddressRefused(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(counting(&hits, "text/plain", "secret"))
	defer target.Close()
	var proxied atomic.Int32
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		http.Redirect(w, r, target.URL+"/", http.StatusFound)
	}))
	defer px.Close()

	res := runFetch(t, fetchTool(t, Config{ProxyURL: px.URL}), FetchInput{URL: "http://93.184.216.34/"})
	if !res.IsError || !strings.Contains(res.Flatten(), "blocked") {
		t.Fatalf("result = %+v, want blocked error", res)
	}
	if n := proxied.Load(); n != 1 {
		t.Fatalf("proxy received %d requests, want only the first hop", n)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("redirect target hit %d times", n)
	}
}

// idleCloser records CloseIdleConnections calls.
type idleCloser struct {
	http.RoundTripper
	closed bool
}

func (c *idleCloser) CloseIdleConnections() { c.closed = true }

func TestGuardTransportClosesIdleConnections(t *testing.T) {
	next := &idleCloser{}
	client := &http.Client{Transport: &guardTransport{next: next}}
	client.CloseIdleConnections()
	if !next.closed {
		t.Fatal("CloseIdleConnections not forwarded")
	}
}
