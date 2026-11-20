package web

import (
	"context"
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
	ctx   context.Context
	base  *url.URL
	buf   []byte
	limit int // most bytes buf holds
	// full stops the conversion: buf reached limit, or ctx is done.
	full      bool
	truncated bool  // output was cut at limit
	err       error // ctx's error, once it is done
	nodes     int   // nodes converted, for context checks
	// tables counts the enclosing tables.
	tables int
	// prefix is the stack of line prefixes: quote markers and list
	// indentation, at most maxNesting deep.
	prefix []string
	// lists holds the item count of each enclosing list; -1 marks an
	// unordered list.
	lists []int
	// inLink counts the enclosing links, whose text escapes brackets.
	inLink int

	pending     int    // line breaks owed before the next content
	breakPrefix string // prefix of the blank line a paragraph break writes
	space       bool   // a space is owed before the next word
	needPrefix  bool   // the current line has no prefix yet
	lineStart   bool   // the current line holds nothing but its prefix
	afterOpen   bool   // the last output is an opening inline marker
	markerOnly  bool   // the current line holds nothing but a list marker
}

func newConverter(ctx context.Context, base *url.URL, limit int) *converter {
	return &converter{ctx: ctx, base: base, limit: limit, needPrefix: true, lineStart: true}
}

// sub returns a converter for content rendered apart, such as a table
// cell, sharing c's context, node count and remaining room.
func (c *converter) sub() *converter {
	s := newConverter(c.ctx, c.base, c.limit-len(c.buf))
	s.nodes, s.tables = c.nodes, c.tables
	return s
}

// adopt takes back the node count and stop state of sub.
func (c *converter) adopt(sub *converter) {
	c.nodes = sub.nodes
	if sub.err != nil {
		c.err, c.full = sub.err, true
	}
	if sub.truncated {
		c.truncated, c.full = true, true
	}
}

// write appends s, cutting it at a character boundary where it would take
// buf past limit; the conversion then stops.
func (c *converter) write(s string) {
	if c.full {
		return
	}
	if room := c.limit - len(c.buf); len(s) > room {
		for room > 0 && !utf8.RuneStart(s[room]) {
			room--
		}
		s = s[:room]
		c.full, c.truncated = true, true
	}
	c.buf = append(c.buf, s...)
}

// pushPrefix adds p to the line prefix unless it is maxNesting deep, and
// reports whether it did; popPrefix undoes it.
func (c *converter) pushPrefix(p string) bool {
	if len(c.prefix) >= maxNesting {
		return false
	}
	c.prefix = append(c.prefix, p)
	return true
}

func (c *converter) popPrefix(pushed bool) {
	if pushed {
		c.prefix = c.prefix[:len(c.prefix)-1]
	}
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
		if c.full {
			return
		}
		if i > 0 {
			c.space = true
		}
		if c.inLink > 0 {
			w = escapeBrackets(w)
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
		c.write(" ")
	}
	c.write(s)
	c.space, c.afterOpen, c.lineStart, c.markerOnly = false, false, false, false
}

// open writes an opening marker; the word after it takes no space.
func (c *converter) open(marker string) {
	c.word(marker)
	c.afterOpen = true
}

// close writes a closing marker; an owed space moves after it.
func (c *converter) close(marker string) {
	c.write(marker)
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
		c.write("\n")
		if c.pending >= paragraph {
			c.write(c.breakPrefix)
			c.write("\n")
		}
		c.needPrefix, c.lineStart, c.space = true, true, false
	}
	c.pending = 0
	if c.needPrefix {
		c.write(strings.Join(c.prefix, ""))
		c.needPrefix = false
	}
}

// trimLine drops trailing spaces from the current line.
func (c *converter) trimLine() {
	for len(c.buf) > 0 && (c.buf[len(c.buf)-1] == ' ' || c.buf[len(c.buf)-1] == '\t') {
		c.buf = c.buf[:len(c.buf)-1]
	}
}
