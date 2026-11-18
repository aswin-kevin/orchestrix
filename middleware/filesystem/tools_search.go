package filesystem

import (
	"context"
	"fmt"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// maxGlobResults caps how many paths glob returns.
const maxGlobResults = 1000

type globInput struct {
	Pattern string `json:"pattern" desc:"Glob pattern such as \"**/*.go\" or \"src/*.ts\"."`
	Path    string `json:"path,omitempty" desc:"Directory to search under. Defaults to \"/\"."`
}

func (m *middleware) globTool() tool.Tool {
	return tool.New(ToolGlob,
		m.description(ToolGlob, fmt.Sprintf("Find files by name pattern (supports ** for any depth). Returns matching paths one per line, at most %d.", maxGlobResults)),
		m.glob,
		tool.WithReadOnly(), tool.WithConcurrent(),
		tool.WithPaths(accessFor(permission.OpRead, func(in globInput) string { return in.Path })),
	)
}

func (m *middleware) glob(ctx context.Context, in globInput, c *tool.Call) (tool.Result, error) {
	b, err := m.backendFor(c)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	path := orRoot(in.Path)
	files, err := b.Glob(ctx, in.Pattern, path)
	if err != nil {
		return errorResult(err, path), nil
	}
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

type grepInput struct {
	Pattern    string `json:"pattern" desc:"Regular expression (RE2 syntax), or plain text when literal is set."`
	Path       string `json:"path,omitempty" desc:"Directory or file to search. Defaults to \"/\"."`
	Glob       string `json:"glob,omitempty" desc:"Only search files whose path matches this glob, e.g. \"*.go\"."`
	Mode       string `json:"mode,omitempty" enum:"files_with_matches,content,count" desc:"files_with_matches (default) lists paths; content lists path:line:text; count lists path:count."`
	IgnoreCase bool   `json:"ignore_case,omitempty" desc:"Match case-insensitively."`
	Literal    bool   `json:"literal,omitempty" desc:"Treat pattern as plain text, not a regular expression."`
}

func (m *middleware) grepTool() tool.Tool {
	return tool.New(ToolGrep,
		m.description(ToolGrep, fmt.Sprintf("Search file contents with a regular expression. Results are capped at %d entries. Binary and very large files are skipped.", backend.DefaultMaxResults)),
		m.grep,
		tool.WithReadOnly(), tool.WithConcurrent(),
		tool.WithPaths(accessFor(permission.OpRead, func(in grepInput) string { return in.Path })),
	)
}

func (m *middleware) grep(ctx context.Context, in grepInput, c *tool.Call) (tool.Result, error) {
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
		MaxResults: backend.DefaultMaxResults,
	})
	if err != nil {
		return errorResult(err, path), nil
	}
	if len(matches) == 0 {
		return tool.Text("no matches"), nil
	}
	out := m.formatMatches(matches, backend.GrepMode(in.Mode))
	if len(matches) >= backend.DefaultMaxResults {
		out += fmt.Sprintf("\n[results capped at %d; narrow the pattern, path or glob]", backend.DefaultMaxResults)
	}
	return tool.Text(out), nil
}

func (m *middleware) formatMatches(matches []backend.GrepMatch, mode backend.GrepMode) string {
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
