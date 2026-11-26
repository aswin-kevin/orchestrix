package filesystem

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

const (
	lineTruncatedMarker = "… [line truncated]"
	emptyFileText       = "<file is empty>"
	emptyDirText        = "<empty directory>"
	mediaTypePDF        = "application/pdf"
)

// MaxMediaBytes is the largest image or PDF read_file attaches for the model;
// a bigger one gets the text placeholder instead, since a multi-megabyte
// attachment would swamp the request.
const MaxMediaBytes = 5 << 20

// LsInput is the input of ls.
type LsInput struct {
	// Path is a tool parameter. Directory to list, absolute from the workspace root. Defaults to "/".
	Path string `json:"path,omitempty" desc:"Directory to list, absolute from the workspace root. Defaults to \"/\"."`
}

func (m *Middleware) lsTool() tool.Tool {
	return tool.New(ToolLs,
		m.description(ToolLs, "List the entries of a directory, one per line. Directories end with \"/\". Lists direct children only; use glob to search recursively."),
		m.ls,
		tool.WithReadOnly(), tool.WithConcurrent(),
		tool.WithPaths(accessFor(permission.OpRead, func(in LsInput) string { return in.Path })),
	)
}

func (m *Middleware) ls(ctx context.Context, in LsInput, c *tool.Call) (tool.Result, error) {
	b, err := m.backendFor(c)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	path := orRoot(in.Path)
	entries, err := b.Ls(ctx, path)
	if err != nil {
		return errorResult(err, path), nil
	}
	entries = visibleEntries(c, entries)
	if len(entries) == 0 {
		return tool.Text(emptyDirText), nil
	}
	return tool.Text(formatEntries(entries)), nil
}

// visibleEntries drops the entries the run's read rules hide; see
// tool.Call.ReadVisible.
func visibleEntries(c *tool.Call, entries []backend.FileInfo) []backend.FileInfo {
	return slices.DeleteFunc(entries, func(e backend.FileInfo) bool { return !c.Visible(e.Path) })
}

func formatEntries(entries []backend.FileInfo) string {
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = e.Path
		if e.IsDir {
			lines[i] += "/"
		}
	}
	return strings.Join(lines, "\n")
}

// ReadFileInput is the input of read_file.
type ReadFileInput struct {
	// Path is a tool parameter. File to read, absolute from the workspace root.
	Path string `json:"path" desc:"File to read, absolute from the workspace root."`
	// Offset is a tool parameter. 1-based line number to start from. Defaults to 1.
	Offset int `json:"offset,omitempty" desc:"1-based line number to start from. Defaults to 1."`
	// Limit is a tool parameter. Maximum number of lines to return. Defaults to the configured page size.
	Limit int `json:"limit,omitempty" desc:"Maximum number of lines to return. Defaults to the configured page size."`
}

func (m *Middleware) readFileTool() tool.Tool {
	return tool.New(ToolReadFile,
		m.description(ToolReadFile, fmt.Sprintf("Read a file with numbered lines. Returns at most %d lines by default and says when more remain; page with offset and limit. Lines longer than %d characters are truncated. Images and PDFs are returned as attachments when the model supports them. A file must be read before it can be edited or overwritten.", m.cfg.ReadLimit, m.cfg.MaxLineChars)),
		m.readFile,
		tool.WithReadOnly(), tool.WithConcurrent(),
		tool.WithPaths(accessFor(permission.OpRead, func(in ReadFileInput) string { return in.Path })),
	)
}

func (m *Middleware) readFile(ctx context.Context, in ReadFileInput, c *tool.Call) (tool.Result, error) {
	b, err := m.backendFor(c)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = m.cfg.ReadLimit
	}
	res, err := b.Read(ctx, in.Path, max(in.Offset-1, 0), limit)
	if err != nil {
		return errorResult(err, in.Path), nil
	}
	if path, err := backend.Clean(in.Path); err == nil {
		markRead(c, path, res.ModTime)
	}
	if res.Binary {
		return binaryResult(res, c.Profile), nil
	}
	return tool.Text(m.formatPage(res)), nil
}

// formatPage renders a page in `cat -n` style, with a footer saying how to
// continue when lines remain.
func (m *Middleware) formatPage(res backend.ReadResult) string {
	if res.TotalLines == 0 {
		return emptyFileText
	}
	if len(res.Lines) == 0 {
		return fmt.Sprintf("offset is past the end of the file (%d lines)", res.TotalLines)
	}
	var sb strings.Builder
	for i, line := range res.Lines {
		fmt.Fprintf(&sb, "%6d\t%s\n", res.StartLine+i+1, truncateLine(line, m.cfg.MaxLineChars))
	}
	last := res.StartLine + len(res.Lines)
	if last < res.TotalLines {
		fmt.Fprintf(&sb, "[showing lines %d-%d of %d; use offset=%d to continue]\n", res.StartLine+1, last, res.TotalLines, last+1)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// truncateLine caps line at limit bytes, cutting at a rune boundary so no
// UTF-8 sequence is split; one huge line cannot flood the context.
func truncateLine(line string, limit int) string {
	if len(line) <= limit {
		return line
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + lineTruncatedMarker
}

// binaryResult returns images and PDFs up to MaxMediaBytes as content blocks
// when the model can take them, and a plain notice otherwise.
func binaryResult(res backend.ReadResult, profile llm.Profile) tool.Result {
	if len(res.Data) <= MaxMediaBytes {
		src := &llm.Source{Type: "base64", MediaType: res.MediaType, Data: base64.StdEncoding.EncodeToString(res.Data)}
		switch {
		case profile.Images && strings.HasPrefix(res.MediaType, "image/"):
			return tool.Result{Content: []llm.Block{{Type: llm.BlockImage, Source: src}}}
		case profile.PDFs && res.MediaType == mediaTypePDF:
			return tool.Result{Content: []llm.Block{{Type: llm.BlockDocument, Source: src}}}
		}
	}
	return tool.Text(fmt.Sprintf("binary file (%s, %d bytes) — cannot display", res.MediaType, len(res.Data)))
}
