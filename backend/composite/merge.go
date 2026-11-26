package composite

import (
	"context"
	"errors"
	"path"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/internal/pathutil"
)

// mergeLs lists p from the backend serving it, hides entries owned by deeper
// mounts and adds a directory for each mount below p.
func (c *composite) mergeLs(ctx context.Context, p string) ([]backend.FileInfo, error) {
	below := c.under(p)
	src, rel := c.source(p)
	infos, err := src.backend.Ls(ctx, rel)
	if err != nil {
		// A mount ancestor is a directory even when the default backend has
		// nothing there (or a file of that name).
		if !missingDir(err, below) {
			return nil, err
		}
		infos = nil
	}
	synthetic := map[string]bool{}
	for _, m := range below {
		rest := strings.TrimPrefix(m.prefix, strings.TrimSuffix(p, "/")+"/")
		first, _, _ := strings.Cut(rest, "/")
		synthetic[path.Join(p, first)] = true
	}
	out := make([]backend.FileInfo, 0, len(infos)+len(synthetic))
	for _, fi := range infos {
		fi.Path = join(src.prefix, fi.Path)
		if c.owns(src.prefix, fi.Path) && !synthetic[fi.Path] {
			out = append(out, fi)
		}
	}
	for dir := range synthetic {
		out = append(out, backend.FileInfo{Path: dir, IsDir: true})
	}
	slices.SortFunc(out, func(a, b backend.FileInfo) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// Glob finds files under base matching pattern across the default backend and
// every mount, newest first. A base at or below a mount is served wholly by
// that mount.
func (c *composite) Glob(ctx context.Context, pattern, base string) ([]backend.FileInfo, error) {
	base, err := pathutil.CleanBase(base)
	if err != nil {
		return nil, err
	}
	abs := pattern
	if !strings.HasPrefix(pattern, "/") {
		abs = path.Join(base, pattern)
	}
	below := c.under(base)
	src, rel := c.source(base)
	infos, err := src.backend.Glob(ctx, srcPattern(pattern, src), rel)
	if err != nil && !missingDir(err, below) {
		return nil, err
	}
	var out []backend.FileInfo
	for _, fi := range infos {
		fi.Path = join(src.prefix, fi.Path)
		if c.keeps(src.prefix, fi.Path) && backend.MatchGlob(abs, fi.Path) {
			out = append(out, fi)
		}
	}
	for _, m := range below {
		if out, err = c.globMount(ctx, m, "/", abs, out); err != nil {
			return nil, err
		}
	}
	backend.SortGlobResults(out)
	return out, nil
}

// srcPattern is the pattern to ask src for: the caller's own for the default
// backend, everything for a mount (matching then happens on composite paths,
// so patterns reaching across the mount point behave as in one namespace).
func srcPattern(pattern string, src mount) string {
	if src.prefix == "" {
		return pattern
	}
	return "**"
}

// globMount appends the files of mount m below rel that it owns and that match
// the absolute pattern.
func (c *composite) globMount(ctx context.Context, m mount, rel, abs string, out []backend.FileInfo) ([]backend.FileInfo, error) {
	// Enumerating the whole mount is O(files in the mount) per call; Backend
	// offers no pattern translation across a prefix, so this is the exact way.
	infos, err := m.backend.Glob(ctx, "**", rel)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) && rel == "/" {
			return out, nil
		}
		return nil, err
	}
	for _, fi := range infos {
		fi.Path = join(m.prefix, fi.Path)
		if c.keeps(m.prefix, fi.Path) && backend.MatchGlob(abs, fi.Path) {
			out = append(out, fi)
		}
	}
	return out, nil
}

// Grep searches file contents across the backend serving opts.Path and every
// mount below it; results are ordered by path then line and cut at
// opts.MaxResults.
func (c *composite) Grep(ctx context.Context, pattern string, opts backend.GrepOptions) ([]backend.GrepMatch, error) {
	base, err := pathutil.CleanBase(opts.Path)
	if err != nil {
		return nil, err
	}
	limit := opts.MaxResults
	if limit <= 0 {
		limit = backend.DefaultMaxResults
	}
	absGlob := backend.AnchorGlob(opts.Glob, base)
	below := c.under(base)
	src, rel := c.source(base)
	out, err := c.grepMount(ctx, src, rel, pattern, opts, absGlob, limit)
	if err != nil && !missingDir(err, below) {
		return nil, err
	}
	for _, m := range below {
		more, err := c.grepMount(ctx, m, "/", pattern, opts, absGlob, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	sortMatches(out)
	return out[:min(len(out), limit)], nil
}

// grepMount searches the backend of src (the default backend when its prefix
// is empty) below rel and keeps the results src owns, re-prefixed. absGlob is
// the file filter in composite paths, translated into the mount's namespace.
// When shadowed results leave fewer than limit while the backend hit its cap,
// it asks again with a larger cap so they cannot starve the answer.
func (c *composite) grepMount(ctx context.Context, src mount, rel, pattern string, opts backend.GrepOptions, absGlob string, limit int) ([]backend.GrepMatch, error) {
	opts.Path = rel
	if src.prefix != "" {
		// The mount sees no filter: matching on composite paths is the only
		// way every glob form (including "**" across the mount point) agrees
		// with Glob. The refill loop below covers the filtered-out matches.
		opts.Glob = ""
	}
	asked := limit
	for {
		opts.MaxResults = asked
		got, err := src.backend.Grep(ctx, pattern, opts)
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) && src.prefix != "" && rel == "/" {
				return nil, nil
			}
			return nil, err
		}
		var kept []backend.GrepMatch
		for _, gm := range got {
			gm.Path = join(src.prefix, gm.Path)
			if c.keeps(src.prefix, gm.Path) && (src.prefix == "" || absGlob == "" || backend.MatchGlob(absGlob, gm.Path)) {
				kept = append(kept, gm)
			}
		}
		if len(kept) >= limit || len(got) < asked {
			return kept, nil
		}
		asked *= 2
	}
}

func sortMatches(ms []backend.GrepMatch) {
	slices.SortStableFunc(ms, func(a, b backend.GrepMatch) int {
		if c := backend.ComparePaths(a.Path, b.Path); c != 0 {
			return c
		}
		return a.Line - b.Line
	})
}

// missingDir reports whether err only says the default backend has no
// directory (or has a file) where mounts below make one: it is then not an
// error.
func missingDir(err error, below []mount) bool {
	return len(below) > 0 && (errors.Is(err, backend.ErrNotFound) || errors.Is(err, backend.ErrNotDir))
}
