package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// dialPolicy decides whether a connection to address, a literal "ip:port",
// may be made. A nil error allows it.
type dialPolicy func(network, address string) error

// blockedPrefixes are the non-public ranges the netip predicates do not
// cover.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, includes broadcast
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
	netip.MustParsePrefix("2001::/32"),      // Teredo tunnels
}

// Ranges of IPv6 addresses that carry an IPv4 address.
var (
	// ipv4CompatPrefix holds the deprecated IPv4-compatible addresses,
	// which carry the IPv4 address in their last 32 bits.
	ipv4CompatPrefix = netip.MustParsePrefix("::/96")
	// nat64Prefix carries the IPv4 address in its last 32 bits.
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	// sixToFourPrefix carries the IPv4 address in bits 16 to 48.
	sixToFourPrefix = netip.MustParsePrefix("2002::/16")
)

// blockedIP reports whether ip is not a public unicast address: loopback,
// private, link-local, multicast, unspecified, one of blockedPrefixes, or an
// IPv4-mapped, IPv4-compatible, NAT64 or 6to4 address whose IPv4 address is
// blocked. An
// invalid address is blocked.
func blockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if v4, ok := embeddedIPv4(ip); ok {
		return blockedIP(v4)
	}
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// embeddedIPv4 returns the IPv4 address a NAT64 or 6to4 address carries.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	b := ip.As16()
	switch {
	case !ip.Is6():
		return netip.Addr{}, false
	case nat64Prefix.Contains(ip), ipv4CompatPrefix.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFourPrefix.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

// publicOnly is the dial policy that refuses every blocked address.
func publicOnly(_, address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable address %q", ErrBlockedAddress, address)
	}
	if ip := ap.Addr().Unmap(); blockedIP(ip) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, ip.WithZone(""))
	}
	return nil
}

// localHostName reports whether host is localhost or a name under it, which
// resolve to loopback by convention.
func localHostName(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// guardTransport checks the host of every request, each redirect hop
// included, before passing it on, and sets the User-Agent of requests that
// carry none.
//
// Without a proxy it only refuses local host names: the dial policy sees the
// address actually connected to. Through a proxy the transport connects only
// to the proxy, so guardTransport also resolves the host and refuses blocked
// answers. The proxy then resolves the host again for the connection it
// makes, so that check cannot rule out a DNS answer that changes in between.
type guardTransport struct {
	next      http.RoundTripper
	userAgent string
	// blockPrivate refuses local host names.
	blockPrivate bool
	// resolver, when non-nil, resolves every host and refuses blocked
	// answers.
	resolver *net.Resolver
}

// RoundTrip implements http.RoundTripper.
func (g *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := g.check(req.Context(), req.URL.Hostname()); err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	if req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", g.userAgent)
	}
	return g.next.RoundTrip(req)
}

// CloseIdleConnections closes the idle connections of the wrapped
// transport, so http.Client.CloseIdleConnections reaches it.
func (g *guardTransport) CloseIdleConnections() {
	if c, ok := g.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// check returns an error wrapping ErrBlockedAddress when host may not be
// contacted.
func (g *guardTransport) check(ctx context.Context, host string) error {
	if !g.blockPrivate {
		return nil
	}
	if localHostName(host) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, host)
	}
	if g.resolver == nil {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if blockedIP(ip) {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, ip.Unmap().WithZone(""))
		}
		return nil
	}
	addrs, err := g.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range addrs {
		if blockedIP(ip) {
			return fmt.Errorf("%w: %s resolves to %s", ErrBlockedAddress, host, ip.Unmap().WithZone(""))
		}
	}
	return nil
}
