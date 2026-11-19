package web

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// errCredentials refuses URLs that carry a user name or password.
var errCredentials = errors.New("credentials in URLs are not supported")

// canonicalURL parses an http or https URL into the one form that is both
// the permission subject of web_fetch and the URL requested, so a rule sees
// exactly what is fetched. It lowercases the scheme and host, strips
// trailing dots from the host, writes the port in decimal and drops it when
// it is the scheme's default, drops the fragment, turns an empty path into
// "/", decodes escaped unreserved characters and resolves "." and ".." path
// segments, keeping a trailing slash. It refuses URLs with credentials and
// paths whose ".." segments climb above the root.
func canonicalURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !webScheme(strings.ToLower(u.Scheme)) || u.Opaque != "" {
		return nil, fmt.Errorf("invalid URL %q: want an http or https URL with a host", raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", raw, errCredentials)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return nil, fmt.Errorf("invalid URL %q: want an http or https URL with a host", raw)
	}
	port, err := canonicalPort(u)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch {
	case port == "" || port == defaultPort(u.Scheme):
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		u.Host = host
	default:
		u.Host = net.JoinHostPort(host, port)
	}
	escaped, err := resolveDots(u.EscapedPath())
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Path, err = url.PathUnescape(escaped); err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	u.RawPath = escaped
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// maxPort is the largest TCP port.
const maxPort = 65535

// canonicalPort returns the port of u in decimal without leading zeros, or
// "" when u names none. A colon without a port, and a port outside
// 1..maxPort, are errors.
func canonicalPort(u *url.URL) (string, error) {
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return "", errors.New("empty port")
		}
		return "", nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > maxPort {
		return "", fmt.Errorf("invalid port %q", port)
	}
	return strconv.Itoa(n), nil
}

// defaultPort returns the port scheme implies.
func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// normalizeEscapes decodes the percent-escapes of unreserved characters
// (letters, digits and "-._~"), which mean the same as the characters
// themselves, and uppercases the hex digits of the escapes it keeps, whose
// characters are reserved or unsafe (so %2F stays an escaped slash).
func normalizeEscapes(seg string) string {
	if !strings.Contains(seg, "%") {
		return seg
	}
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		if seg[i] != '%' || i+2 >= len(seg) {
			b.WriteByte(seg[i])
			continue
		}
		v, err := strconv.ParseUint(seg[i+1:i+3], 16, 8)
		switch {
		case err != nil:
			b.WriteByte(seg[i])
			continue
		case unreserved(byte(v)):
			b.WriteByte(byte(v))
		default:
			b.WriteString(strings.ToUpper(seg[i : i+3]))
		}
		i += 2
	}
	return b.String()
}

// unreserved reports whether c may appear in a URL unescaped (RFC 3986).
func unreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// resolveDots removes the "." and ".." segments of an escaped absolute
// path after normalizeEscapes, so encoded dots count too. An empty path becomes
// "/", and a path ending in a dot segment keeps a trailing slash.
func resolveDots(escaped string) (string, error) {
	if escaped == "" {
		return "/", nil
	}
	segs := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		seg = normalizeEscapes(seg)
		last := i == len(segs)-1
		switch dec, _ := url.PathUnescape(seg); dec {
		case ".":
		case "..":
			if len(out) == 0 {
				return "", errors.New("path escapes the root")
			}
			out = out[:len(out)-1]
		default:
			out = append(out, seg)
			continue
		}
		if last {
			out = append(out, "") // keep the directory's trailing slash
		}
	}
	return "/" + strings.Join(out, "/"), nil
}
