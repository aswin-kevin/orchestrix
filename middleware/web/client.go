package web

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"
)

// Connection settings of the transport NewHTTPClient builds.
const (
	dialTimeout           = 10 * time.Second
	keepAlive             = 30 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	idleConnTimeout       = 90 * time.Second
	maxIdleConns          = 32
	expectContinueTimeout = time.Second
)

// ClientConfig configures NewHTTPClient.
type ClientConfig struct {
	// Timeout bounds each request, body included; 0 means DefaultTimeout.
	// Negative is an error.
	Timeout time.Duration
	// MaxRedirects is how many redirects a request follows; 0 means
	// DefaultMaxRedirects. Negative is an error.
	MaxRedirects int
	// ProxyURL is the http or https proxy every request goes through.
	// Empty means no proxy; the proxy environment variables are always
	// ignored.
	ProxyURL string
	// CACertFiles are PEM files of extra root certificates, added to the
	// system pool. Nil trusts the system pool only.
	CACertFiles []string
	// AllowPrivateNetworks permits loopback, private, link-local, CGNAT and
	// other non-public addresses. False refuses them.
	AllowPrivateNetworks bool
	// UserAgent is set on requests that carry none; empty means
	// DefaultUserAgent.
	UserAgent string
}

// NewHTTPClient returns a client with its own transport that enforces the
// network policy of cc: unless AllowPrivateNetworks is set, every connection
// is checked at connect time against the literal address being dialled, so
// each redirect hop and each DNS answer is covered, and local host names are
// refused before resolving. With a proxy the transport connects only to the
// proxy, and each target host is resolved and checked before the request is
// sent; the proxy makes the final resolution itself. Redirects beyond
// MaxRedirects or to a scheme other than http and https fail. It fails when
// Timeout or MaxRedirects is negative, when ProxyURL is not an http or https
// URL, and when a CA file cannot be read or holds no PEM certificate.
func NewHTTPClient(cc ClientConfig) (*http.Client, error) {
	var policy dialPolicy
	if !cc.AllowPrivateNetworks {
		policy = publicOnly
	}
	return newClient(cc, policy)
}

// newClient builds the client of NewHTTPClient with policy (nil allows every
// address) in place of the one cc selects.
func newClient(cc ClientConfig, policy dialPolicy) (*http.Client, error) {
	if err := checkClientLimits(cc.Timeout, cc.MaxRedirects); err != nil {
		return nil, err
	}
	proxy, err := parseProxy(cc.ProxyURL)
	if err != nil {
		return nil, err
	}
	roots, err := loadRoots(cc.CACertFiles)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext:           dialer(policy, proxy),
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxIdleConns,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
	}
	guard := &guardTransport{
		next:         tr,
		userAgent:    cmp.Or(cc.UserAgent, DefaultUserAgent),
		blockPrivate: !cc.AllowPrivateNetworks,
	}
	if proxy != nil {
		tr.Proxy = http.ProxyURL(proxy)
		if guard.blockPrivate {
			guard.resolver = net.DefaultResolver
		}
	}
	return &http.Client{
		Transport:     guard,
		Timeout:       cmp.Or(cc.Timeout, DefaultTimeout),
		CheckRedirect: checkRedirect(cmp.Or(cc.MaxRedirects, DefaultMaxRedirects)),
	}, nil
}

// checkClientLimits rejects a negative timeout or redirect limit.
func checkClientLimits(timeout time.Duration, maxRedirects int) error {
	switch {
	case timeout < 0:
		return errors.New("web: Timeout is negative")
	case maxRedirects < 0:
		return errors.New("web: MaxRedirects is negative")
	}
	return nil
}

// dialer returns the transport's DialContext. Connections to the proxy's
// own address skip policy; every other connection is checked against the
// literal address being dialled, after DNS resolution.
func dialer(policy dialPolicy, proxy *url.URL) func(ctx context.Context, network, addr string) (net.Conn, error) {
	plain := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}
	guarded := *plain
	if policy != nil {
		guarded.Control = func(network, address string, _ syscall.RawConn) error {
			return policy(network, address)
		}
	}
	proxyAddr := ""
	if proxy != nil {
		proxyAddr = hostPort(proxy)
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if proxyAddr != "" && addr == proxyAddr {
			return plain.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}
}

// hostPort returns the "host:port" the transport dials for u, with the
// scheme's default port when u has none.
func hostPort(u *url.URL) string {
	port := u.Port()
	switch {
	case port != "":
	case u.Scheme == "https":
		port = "443"
	default:
		port = "80"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// checkRedirect fails a redirect beyond max hops or to a scheme other than
// http and https.
func checkRedirect(max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > max {
			return fmt.Errorf("too many redirects (limit %d)", max)
		}
		if !webScheme(req.URL.Scheme) {
			return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
		}
		return nil
	}
}

// parseProxy parses raw, returning nil for "".
func parseProxy(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, fmt.Errorf("web: proxy URL: %w", err)
	case !webScheme(u.Scheme) || u.Host == "":
		return nil, fmt.Errorf("web: proxy URL %q: want http:// or https:// with a host", raw)
	}
	return u, nil
}

// loadRoots returns the system pool plus the certificates of files, or nil
// (the system pool) when files is empty.
func loadRoots(files []string) (*x509.CertPool, error) {
	if len(files) == 0 {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("web: CA file: %w", err)
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("web: CA file %s: %w", f, errNoPEM)
		}
	}
	return pool, nil
}

// errNoPEM reports a CA file without a single PEM certificate.
var errNoPEM = errors.New("no PEM certificates")

// webScheme reports whether scheme is http or https.
func webScheme(scheme string) bool { return scheme == "http" || scheme == "https" }

// positive returns v, or def when v is not positive. It is for model input,
// which no constructor has validated.
func positive(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
