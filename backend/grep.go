package backend

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// DefaultMaxResults is the Grep result cap used when GrepOptions.MaxResults
// is not set.
const DefaultMaxResults = 1000

// MaxGrepFileBytes is the largest file Grep searches; bigger files are skipped
// so one huge file cannot dominate memory or time.
const MaxGrepFileBytes = 10 << 20

// LineMatch is one matching line of a file.
type LineMatch struct {
	Line int // 1-based
	// Text is the content of the matching line.
	Text string
}

// Grepper holds the search semantics shared by every backend: pattern
// compilation, glob filtering, binary skipping, output modes and result caps.
// A backend walks its files and feeds them in; Grepper decides what to report.
type Grepper struct {
	re      *regexp.Regexp
	mode    GrepMode
	max     int
	glob    string
	results []GrepMatch
}

// NewGrepper prepares a search. base is the cleaned virtual path being
// searched; a relative opts.Glob containing a slash is anchored there, and one
// without a slash matches file names at any depth.
func NewGrepper(pattern string, opts GrepOptions, base string) (*Grepper, error) {
	if opts.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	if opts.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("backend: grep pattern: %w", err)
	}
	mode := opts.Mode
	if mode == "" {
		mode = GrepFiles
	}
	if mode != GrepFiles && mode != GrepContent && mode != GrepCount {
		return nil, fmt.Errorf("backend: unknown grep mode %q", mode)
	}
	limit := opts.MaxResults
	if limit <= 0 {
		limit = DefaultMaxResults
	}
	return &Grepper{re: re, mode: mode, max: limit, glob: AnchorGlob(opts.Glob, base)}, nil
}

// AnchorGlob resolves a Grep file filter against the searched base: absolute
// and empty globs are unchanged, a name-only glob matches at any depth, and
// one containing a slash is joined to base.
func AnchorGlob(glob, base string) string {
	switch {
	case glob == "", strings.HasPrefix(glob, "/"):
		return glob
	case !strings.Contains(glob, "/"):
		return "**/" + glob
	default:
		return path.Join(base, glob)
	}
}

// WantsPath reports whether the glob filter admits the file at virtual path p.
func (g *Grepper) WantsPath(p string) bool {
	return g.glob == "" || MatchGlob(g.glob, p)
}

// AddFile searches one file's content, skipping binary files and files over
// MaxGrepFileBytes. It reports
// whether the result cap has been reached and the walk can stop.
func (g *Grepper) AddFile(p string, content []byte) (full bool) {
	if len(content) > MaxGrepFileBytes || IsBinary(content) {
		return false
	}
	return g.AddMatches(p, g.findLines(content))
}

// AddMatches records the matching lines of one file (as found by a search
// engine other than Grepper's own regexp). It reports whether the result cap
// has been reached.
func (g *Grepper) AddMatches(p string, lines []LineMatch) (full bool) {
	if len(lines) == 0 {
		return false
	}
	switch g.mode {
	case GrepContent:
		for _, lm := range lines {
			g.results = append(g.results, GrepMatch{Path: p, Line: lm.Line, Text: lm.Text})
			if len(g.results) >= g.max {
				return true
			}
		}
		return false
	case GrepCount:
		g.results = append(g.results, GrepMatch{Path: p, Count: len(lines)})
	default:
		g.results = append(g.results, GrepMatch{Path: p})
	}
	return len(g.results) >= g.max
}

// Results returns everything collected so far.
func (g *Grepper) Results() []GrepMatch {
	return g.results
}

func (g *Grepper) findLines(content []byte) []LineMatch {
	var found []LineMatch
	for i, line := range SplitLines(string(content)) {
		if g.re.MatchString(line) {
			found = append(found, LineMatch{Line: i + 1, Text: line})
		}
	}
	return found
}
