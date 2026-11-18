// Package frontmatter parses and renders the YAML block that may open a
// Markdown file, delimited by "---" lines.
package frontmatter

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	fence = "---"
	bom   = "\xef\xbb\xbf"
)

// Parse decodes a leading YAML block delimited by "---" lines into v and
// returns the rest. ok is false (v untouched, body == whole input) when data
// has no frontmatter: it does not open with a fence line or the fence is never
// closed. A UTF-8 BOM and CRLF line endings are accepted; body keeps the
// file's own line endings. Malformed YAML is an error.
func Parse(data []byte, v any) (body string, ok bool, err error) {
	text := strings.TrimPrefix(string(data), bom)
	rest, found := strings.CutPrefix(text, fence)
	if !found {
		return string(data), false, nil
	}
	rest, found = cutLineEnd(rest)
	if !found {
		return string(data), false, nil
	}
	var yamlEnd, bodyStart int
	for pos := 0; ; {
		line, next, more := nextLine(rest, pos)
		if strings.TrimRight(line, "\r") == fence {
			yamlEnd, bodyStart = pos, next
			break
		}
		if !more {
			return string(data), false, nil
		}
		pos = next
	}
	if err := yaml.NewDecoder(strings.NewReader(rest[:yamlEnd])).Decode(v); err != nil && !errors.Is(err, io.EOF) { // io.EOF: an empty block
		return "", false, fmt.Errorf("frontmatter: %w", err)
	}
	return rest[bodyStart:], true, nil
}

// cutLineEnd removes the line terminator that must follow the opening fence;
// found is false when other text follows the fence on its line.
func cutLineEnd(s string) (rest string, found bool) {
	if r, ok := strings.CutPrefix(s, "\r\n"); ok {
		return r, true
	}
	return strings.CutPrefix(s, "\n")
}

// nextLine returns the line of s starting at pos (without its "\n"), the
// offset of the following line, and whether a following line exists.
func nextLine(s string, pos int) (line string, next int, more bool) {
	i := strings.IndexByte(s[pos:], '\n')
	if i < 0 {
		return s[pos:], len(s), false
	}
	return s[pos : pos+i], pos + i + 1, true
}

// Render encodes v as YAML between "---" lines and appends body.
func Render(v any, body string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(fence + "\n")
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	buf.WriteString(fence + "\n")
	buf.WriteString(body)
	return buf.Bytes(), nil
}

// StringList is a list of strings that a YAML document may write either as a
// sequence or as one space-separated string, such as `tools: read grep` or
// `tools: [read, grep]`.
type StringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*l = strings.Fields(n.Value)
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := n.Decode(&items); err != nil {
			return err
		}
		*l = items
		return nil
	}
	return fmt.Errorf("line %d: expected a string or a list of strings", n.Line)
}

// Metadata is a free-form string-to-string map for a document's extra
// fields. Scalar values of any YAML type are kept in their text form.
type Metadata map[string]string
