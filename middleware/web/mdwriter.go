package web

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Pending line breaks before the next content.
const (
	lineBreak  = 1 // start a new line
	paragraph  = 2 // leave one blank line
	maxPending = paragraph
)

// converter renders a node tree as markdown. Line breaks are kept pending
// until the next content, so leading and trailing breaks vanish and at most
// one blank line separates blocks.
type converter struct {
	base *url.URL
	buf  []byte
	// prefix is the stack of line prefixes: quote markers and list
	// indentation.
	prefix []string
	// lists holds the item count of each enclosing list; -1 marks an
	// unordered list.
	lists []int

	pending     int    // line breaks owed before the next content
	breakPrefix string // prefix of the blank line a paragraph break writes
	space       bool   // a space is owed before the next word
	needPrefix  bool   // the current line has no prefix yet
	lineStart   bool   // the current line holds nothing but its prefix
	afterOpen   bool   // the last output is an opening inline marker
	markerOnly  bool   // the current line holds nothing but a list marker
}

func newConverter(base *url.URL) *converter {
	return &converter{base: base, needPrefix: true, lineStart: true}
}

// String returns the markdown written so far.
func (c *converter) String() string { return strings.TrimSpace(string(c.buf)) }

// text writes a text node with its whitespace collapsed.
func (c *converter) text(s string) {
	if s == "" {
		return
	}
	first, _ := utf8.DecodeRuneInString(s)
	if unicode.IsSpace(first) {
		c.space = true
	}
	for i, w := range strings.Fields(s) {
		if i > 0 {
			c.space = true
		}
		c.word(w)
	}
	last, _ := utf8.DecodeLastRuneInString(s)
	if unicode.IsSpace(last) {
		c.space = true
	}
}

// word writes s, preceded by an owed space unless it starts a line or
// follows an opening marker.
func (c *converter) word(s string) {
	c.flush()
	if c.space && !c.lineStart && !c.afterOpen {
		c.buf = append(c.buf, ' ')
	}
	c.buf = append(c.buf, s...)
	c.space, c.afterOpen, c.lineStart, c.markerOnly = false, false, false, false
}

// open writes an opening marker; the word after it takes no space.
func (c *converter) open(marker string) {
	c.word(marker)
	c.afterOpen = true
}

// close writes a closing marker; an owed space moves after it.
func (c *converter) close(marker string) {
	c.buf = append(c.buf, marker...)
	c.afterOpen, c.lineStart = false, false
}

// block owes n line breaks, unless the line holds only a list marker, whose
// content belongs on it.
func (c *converter) block(n int) {
	if c.markerOnly {
		return
	}
	c.owe(n)
}

// br owes one more line break, up to a blank line.
func (c *converter) br() {
	if c.markerOnly {
		return
	}
	c.owe(min(c.pending+1, maxPending))
}

// owe owes n line breaks. A blank line between blocks at different quote
// depths takes the prefix of the outer one, so it never shows a stray
// quote marker.
func (c *converter) owe(n int) {
	prefix := strings.TrimRight(strings.Join(c.prefix, ""), " ")
	if n >= paragraph && (c.pending < paragraph || len(prefix) < len(c.breakPrefix)) {
		c.breakPrefix = prefix
	}
	c.pending = max(c.pending, n)
}

// flush writes the owed line breaks and the prefix of the current line. The
// breaks are dropped at the start of the output.
func (c *converter) flush() {
	if c.pending > 0 && len(c.buf) > 0 {
		c.trimLine()
		c.buf = append(c.buf, '\n')
		if c.pending >= paragraph {
			c.buf = append(c.buf, c.breakPrefix...)
			c.buf = append(c.buf, '\n')
		}
		c.needPrefix, c.lineStart, c.space = true, true, false
	}
	c.pending = 0
	if c.needPrefix {
		c.buf = append(c.buf, strings.Join(c.prefix, "")...)
		c.needPrefix = false
	}
}

// trimLine drops trailing spaces from the current line.
func (c *converter) trimLine() {
	for len(c.buf) > 0 && (c.buf[len(c.buf)-1] == ' ' || c.buf[len(c.buf)-1] == '\t') {
		c.buf = c.buf[:len(c.buf)-1]
	}
}
