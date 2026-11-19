package composite

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
)

// mount is a validated Route: Prefix is cleaned and has no trailing slash.
type mount struct {
	prefix  string
	backend backend.Backend
}

// checkRoutes validates routes and returns them ordered longest prefix first.
func checkRoutes(routes []Route) ([]mount, error) {
	mounts := make([]mount, 0, len(routes))
	seen := map[string]bool{}
	for _, r := range routes {
		prefix, err := cleanPrefix(r.Prefix)
		if err != nil {
			return nil, err
		}
		if r.Backend == nil {
			return nil, fmt.Errorf("composite: route %q: nil backend", r.Prefix)
		}
		if seen[prefix] {
			return nil, fmt.Errorf("composite: duplicate route prefix %q", prefix)
		}
		seen[prefix] = true
		mounts = append(mounts, mount{prefix: prefix, backend: r.Backend})
	}
	sortByLength(mounts)
	return mounts, nil
}

func sortByLength(m []mount) {
	slices.SortStableFunc(m, func(a, b mount) int { return len(b.prefix) - len(a.prefix) })
}

// cleanPrefix accepts an absolute, already-clean path other than "/" and drops
// one trailing slash.
func cleanPrefix(p string) (string, error) {
	trimmed := strings.TrimSuffix(p, "/")
	if strings.ContainsRune(p, 0) || !strings.HasPrefix(trimmed, "/") || trimmed == "" || path.Clean(trimmed) != trimmed {
		return "", fmt.Errorf("composite: invalid route prefix %q: want an absolute, cleaned path other than /", p)
	}
	return trimmed, nil
}

// within reports whether p equals dir or lies below it, on whole segments:
// "/memories2" is not within "/memories".
func within(p, dir string) bool {
	if dir == "/" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// resolve returns the mount with the longest prefix that p is within, and p
// relative to it ("/" for the mount point itself). ok is false for def paths.
func (c *composite) resolve(p string) (m mount, rel string, ok bool) {
	for _, m := range c.mounts {
		if within(p, m.prefix) {
			rel = strings.TrimPrefix(p, m.prefix)
			if rel == "" {
				rel = "/"
			}
			return m, rel, true
		}
	}
	return mount{}, "", false
}

// under returns the mounts whose prefix lies strictly below dir.
func (c *composite) under(dir string) []mount {
	var out []mount
	for _, m := range c.mounts {
		if m.prefix != dir && within(m.prefix, dir) {
			out = append(out, m)
		}
	}
	return out
}

// join re-prefixes a path of a mounted backend into the composite namespace.
func join(prefix, rel string) string {
	if prefix == "" {
		return rel // default backend: the namespace is already the composite's
	}
	if rel == "/" || rel == "" {
		return prefix
	}
	return prefix + rel
}

// cleanBase cleans a base directory, defaulting an empty one to the root.
func cleanBase(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	return backend.Clean(p)
}

// source returns the mount serving the directory p (a zero mount, with an empty
// prefix, standing for the default backend) and p in its namespace.
func (c *composite) source(p string) (mount, string) {
	if m, rel, ok := c.resolve(p); ok {
		return m, rel
	}
	return mount{backend: c.def}, p
}

// owns reports whether the mount with the given prefix ("" for the default
// backend) is the one serving the composite path p; paths owned by a deeper
// mount are shadowed.
func (c *composite) owns(prefix, p string) bool {
	m, _, ok := c.resolve(p)
	return (!ok && prefix == "") || (ok && m.prefix == prefix)
}

// isAncestor reports whether p is a proper ancestor of some mount.
func (c *composite) isAncestor(p string) bool { return len(c.under(p)) > 0 }
