package filesystem

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// maxGlobResults caps how many paths glob returns.
const maxGlobResults = 1000

// GlobInput is the input of glob.
type GlobInput struct {
	// Pattern is the glob the paths must match, such as "**/*.go".
	Pattern string `json:"pattern" desc:"Glob pattern such as \"**/*.go\" or \"src/*.ts\"."`
	// Path is the directory searched; empty searches from "/".
	Path string `json:"path,omitempty" desc:"Directory to search under. Defaults to \"/\"."`
}

func (m *Middleware) globTool() tool.Tool {
	return tool.New(ToolGlob,
		m.description(ToolGlob, fmt.Sprintf("Find files by name pattern (supports ** for any depth). Returns matching paths one per line, at most %d.", maxGlobResults)),
		m.glob,
		tool.WithReadOnly(), tool.WithConcurrent(),
		tool.WithPaths(accessFor(permission.OpRead, func(in GlobInput) string { return in.Path })),
	)
}

func (m *Middleware) glob(ctx context.Context, in GlobInput, c *tool.Call) (tool.Result, error) {
	b, err := m.backendFor(c)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	path := orRoot(in.Path)
	files, err := b.Glob(ctx, in.Pattern, path)
	if err != nil {
		return errorResult(err, path), nil
	}
	files = visibleEntries(c, files)
	if len(files) == 0 {
		return tool.Text("no files matched"), nil
	}
	total := len(files)
	if total > maxGlobResults {
		files = files[:maxGlobResults]
	}
	out := formatEntries(files)
	if total > maxGlobResults {
		out += fmt.Sprintf("\n[showing %d of %d matches; narrow the pattern or path]", maxGlobResults, total)
	}
	return tool.Text(out), nil
}

// GrepInput is the input of grep.
type GrepInput struct {
	// Pattern is the RE2 regular expression searched for, or plain text
	// when Literal is set.
	Pattern string `json:"pattern" desc:"Regular expression (RE2 syntax), or plain text when literal is set."`
	// Path is the directory or file searched; empty searches from "/".
	Path string `json:"path,omitempty" desc:"Directory or file to search. Defaults to \"/\"."`
	// Glob, when set, limits the search to files whose path matches it.
	Glob string `json:"glob,omitempty" desc:"Only search files whose path matches this glob, e.g. \"*.go\"."`
	// Mode is what the result lists: "files_with_matches" (the default)
	// the matching paths, "content" each match as path:line:text, and
	// "count" the number of matches per path.
	Mode string `json:"mode,omitempty" enum:"files_with_matches,content,count" desc:"files_with_matches (default) lists paths; content lists path:line:text; count lists path:count."`
	// IgnoreCase makes the match case-insensitive.
	IgnoreCase bool `json:"ignore_case,omitempty" desc:"Match case-insensitively."`
	// Literal makes Pattern plain text rather than a regular expression.
	Literal bool `json:"literal,omitempty" desc:"Treat pattern as plain text, not a regular expression."`
}

func (m *Middleware) grepTool() tool.Tool {
	return tool.New(ToolGrep,
		m.description(ToolGrep, fmt.Sprintf("Search file contents with a regular expression. Results are capped at %d entries. Binary and very large files are skipped.", backend.DefaultMaxResults)),
		m.grep,
		tool.WithReadOnly(), tool.WithConcurrent(),
		tool.WithPaths(accessFor(permission.OpRead, func(in GrepInput) string { return in.Path })),
	)
}

func (m *Middleware) grep(ctx context.Context, in GrepInput, c *tool.Call) (tool.Result, error) {
	b, err := m.backendFor(c)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	path := orRoot(in.Path)
	matches, err := b.Grep(ctx, in.Pattern, backend.GrepOptions{
		Path:       path,
		Glob:       in.Glob,
		Mode:       backend.GrepMode(in.Mode),
		IgnoreCase: in.IgnoreCase,
		Literal:    in.Literal,
		MaxResults: backend.DefaultMaxResults + 1, // one extra reveals truncation
	})
	if err != nil {
		return errorResult(err, path), nil
	}
	// Measured before hidden matches are dropped: they still used up the
	// backend's result budget, so visible matches may be missing.
	capped := len(matches) > backend.DefaultMaxResults
	matches = slices.DeleteFunc(matches, func(match backend.GrepMatch) bool { return !c.Visible(match.Path) })
	if len(matches) == 0 {
		return tool.Text("no matches"), nil
	}
	if len(matches) > backend.DefaultMaxResults {
		matches = matches[:backend.DefaultMaxResults]
	}
	out := m.formatMatches(matches, backend.GrepMode(in.Mode))
	if capped {
		out += fmt.Sprintf("\n[results capped at %d; narrow the pattern, path or glob]", backend.DefaultMaxResults)
	}
	return tool.Text(out), nil
}

func (m *Middleware) formatMatches(matches []backend.GrepMatch, mode backend.GrepMode) string {
	lines := make([]string, len(matches))
	for i, match := range matches {
		switch mode {
		case backend.GrepContent:
			lines[i] = fmt.Sprintf("%s:%d:%s", match.Path, match.Line, truncateLine(match.Text, m.cfg.MaxLineChars))
		case backend.GrepCount:
			lines[i] = fmt.Sprintf("%s:%d", match.Path, match.Count)
		default:
			lines[i] = match.Path
		}
	}
	return strings.Join(lines, "\n")
}
