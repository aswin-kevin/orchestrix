package backend

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// binarySniffBytes is how much of a file is inspected for NUL bytes when
// deciding whether it is binary.
const binarySniffBytes = 8 << 10

// IsBinary reports whether content should be treated as non-text: it has a NUL
// byte in its first 8 KB or is not valid UTF-8.
func IsBinary(content []byte) bool {
	head := content
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	return !utf8.Valid(content)
}

// SplitLines splits text into lines without their terminators. A trailing
// newline does not produce an extra empty line, and "\r\n" endings lose the
// "\r".
func SplitLines(content string) []string {
	if content == "" {
		return nil
	}
	content = strings.TrimSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

// NewReadResult builds the Read result for a file's content: a page of lines
// for text, or the raw bytes for binary files. offset is a 0-based line index;
// limit <= 0 means all remaining lines.
func NewReadResult(content []byte, modTime time.Time, offset, limit int) ReadResult {
	if IsBinary(content) {
		return ReadResult{
			Binary:    true,
			MediaType: http.DetectContentType(content),
			Data:      append([]byte(nil), content...),
			ModTime:   modTime,
		}
	}
	lines := SplitLines(string(content))
	total := len(lines)
	offset = max(offset, 0)
	start := min(offset, total)
	end := total
	if limit > 0 {
		end = min(start+limit, total)
	}
	return ReadResult{
		Lines:      lines[start:end],
		StartLine:  offset,
		TotalLines: total,
		ModTime:    modTime,
	}
}

// ApplyEdit replaces old with replacement in content and reports how many
// replacements were made. It enforces the Backend.Edit error contract so every
// implementation behaves the same.
func ApplyEdit(content []byte, old, replacement string, replaceAll bool) ([]byte, int, error) {
	if old == "" {
		return nil, 0, fmt.Errorf("%w: old text is empty", ErrInvalidEdit)
	}
	if old == replacement {
		return nil, 0, fmt.Errorf("%w: old and new text are identical", ErrInvalidEdit)
	}
	text := string(content)
	count := strings.Count(text, old)
	switch {
	case count == 0:
		return nil, 0, ErrNoMatch
	case count > 1 && !replaceAll:
		return nil, 0, fmt.Errorf("%w: old text occurs %d times", ErrAmbiguous, count)
	}
	return []byte(strings.ReplaceAll(text, old, replacement)), count, nil
}
