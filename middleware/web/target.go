package web

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// errCredentials refuses URLs that carry a user name or password.
var errCredentials = errors.New("credentials in URLs are not supported")

// canonicalURL parses an http or https URL into the one form that is both
// the permission subject of web_fetch and the URL requested, so a rule sees
// exactly what is fetched. It lowercases the scheme and host, strips
// trailing dots from the host, drops the scheme's default port and the
// fragment, turns an empty path into "/" and resolves "." and ".." path
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
	switch port := u.Port(); {
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

// defaultPort returns the port scheme implies.
func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// resolveDots removes the "." and ".." segments of an escaped absolute
// path, recognising them in percent-encoded form too. An empty path becomes
// "/", and a path ending in a dot segment keeps a trailing slash.
func resolveDots(escaped string) (string, error) {
	if escaped == "" {
		return "/", nil
	}
	segs := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
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
