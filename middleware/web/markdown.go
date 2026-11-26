package web

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// droppedElements are left out of the markdown with everything inside them.
var droppedElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
	"svg": true, "iframe": true, "head": true,
}

// blockElements end the paragraph before and after them.
var blockElements = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "header": true,
	"footer": true, "main": true, "nav": true, "aside": true, "figure": true,
	"figcaption": true, "form": true, "address": true, "details": true,
	"summary": true, "dl": true, "dt": true, "dd": true,
}

// headingLevels maps heading elements to their level.
var headingLevels = map[string]int{"h1": 1, "h2": 2, "h3": 3, "h4": 4, "h5": 5, "h6": 6}

// Markdown prefixes and markers.
const (
	quotePrefix  = "> "
	listIndent   = "  "
	bulletMarker = "- "
	codeFence    = "```"
)

const (
	// DefaultMaxMarkdownBytes is how much markdown HTMLToMarkdown writes
	// when its maxBytes is ≤ 0.
	DefaultMaxMarkdownBytes = 1 << 20
	// maxNesting is how many quote markers and list indents a line takes
	// at most; deeper blockquotes and lists add no further prefix.
	maxNesting = 16
	// maxTableNesting is how many tables deep cells are converted as
	// markdown, links included; cells of deeper tables are plain text.
	maxTableNesting = 4
	// checkEvery is how many nodes are converted between checks of the
	// context.
	checkEvery = 1024
)

// HTMLToMarkdown converts an HTML document to markdown and returns its
// title (whitespace collapsed; empty when it has none). Relative links
// and image sources are resolved against the document's <base href>, itself
// resolved against base; a nil base leaves them as they are written.
// Scripts, styles and other non-content elements are dropped, and links to
// javascript: URLs keep only their text.
//
// The markdown is at most maxBytes long (≤ 0 means
// DefaultMaxMarkdownBytes); truncated reports that it was cut there.
// Conversion stops with ctx's error once ctx is done. It also fails on
// documents the parser rejects, such as ones nested more than 512 elements
// deep; the parser's bound on nesting also bounds the conversion's
// recursion.
func HTMLToMarkdown(ctx context.Context, r io.Reader, base *url.URL, maxBytes int) (title, markdown string, truncated bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	doc, err := html.Parse(r)
	if err != nil {
		return "", "", false, err
	}
	if el := find(doc, "title"); el != nil {
		title = collapse(rawText(el))
	}
	if el := find(doc, "base"); el != nil {
		if href, ok := attr(el, "href"); ok {
			base = resolveBase(base, href)
		}
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMarkdownBytes
	}
	c := newConverter(ctx, base, maxBytes)
	c.children(doc)
	if c.err != nil {
		return title, "", false, c.err
	}
	return title, c.String(), c.truncated, nil
}

func (c *converter) children(n *html.Node) {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.node(ch)
	}
}

// elementHandlers converts the elements that need more than their children,
// by tag name. It is filled in init: assigning it directly would make an
// initialization cycle through node.
var elementHandlers map[string]func(*converter, *html.Node)

func init() {
	elementHandlers = map[string]func(*converter, *html.Node){
		"blockquote": (*converter).blockquote,
		"br":         func(c *converter, _ *html.Node) { c.br() },
		"hr":         (*converter).rule,
		"a":          (*converter).link,
		"img":        (*converter).image,
		"ul":         func(c *converter, n *html.Node) { c.list(n, false) },
		"ol":         func(c *converter, n *html.Node) { c.list(n, true) },
		"li":         (*converter).item,
		"pre":        (*converter).pre,
		"code":       (*converter).code,
		"strong":     func(c *converter, n *html.Node) { c.wrap(n, "**") },
		"b":          func(c *converter, n *html.Node) { c.wrap(n, "**") },
		"em":         func(c *converter, n *html.Node) { c.wrap(n, "*") },
		"i":          func(c *converter, n *html.Node) { c.wrap(n, "*") },
		"table":      (*converter).table,
	}
}

func (c *converter) node(n *html.Node) {
	if c.full {
		return
	}
	if c.nodes++; c.nodes%checkEvery == 0 {
		if err := c.ctx.Err(); err != nil {
			c.err, c.full = err, true
			return
		}
	}
	switch n.Type {
	case html.TextNode:
		c.text(n.Data)
		return
	case html.ElementNode:
	default:
		return
	}
	if droppedElements[n.Data] {
		return
	}
	if level, ok := headingLevels[n.Data]; ok {
		c.block(paragraph)
		c.open(strings.Repeat("#", level) + " ")
		c.children(n)
		c.block(paragraph)
		return
	}
	if blockElements[n.Data] {
		c.block(paragraph)
		c.children(n)
		c.block(paragraph)
		return
	}
	if h, ok := elementHandlers[n.Data]; ok {
		h(c, n)
		return
	}
	c.children(n)
}

func (c *converter) blockquote(n *html.Node) {
	c.block(paragraph)
	pushed := c.pushPrefix(quotePrefix)
	c.children(n)
	c.popPrefix(pushed)
	c.block(paragraph)
}

func (c *converter) rule(*html.Node) {
	c.block(paragraph)
	c.word("---")
	c.block(paragraph)
}

func (c *converter) wrap(n *html.Node, marker string) {
	c.open(marker)
	c.children(n)
	c.close(marker)
}

func (c *converter) link(n *html.Node) {
	href, ok := attr(n, "href")
	if !ok || isJavaScript(href) {
		c.children(n)
		return
	}
	c.open("[")
	c.inLink++
	c.children(n)
	c.inLink--
	c.close("](" + destination(c.resolve(href)) + ")")
}

// image writes an image. An inline data: image keeps only its alt text: its
// bytes mean nothing to a reader of the markdown.
func (c *converter) image(n *html.Node) {
	src, ok := attr(n, "src")
	if !ok || src == "" {
		return
	}
	alt, _ := attr(n, "alt")
	if hasScheme(src, "data") {
		c.text(alt)
		return
	}
	c.word("![" + escapeBrackets(collapse(alt)) + "](" + destination(c.resolve(src)) + ")")
}

// destination formats a URL as a markdown link destination, wrapping it in
// angle brackets when spaces or parentheses would end it early.
func destination(u string) string {
	if !strings.ContainsAny(u, " ()<>") {
		return u
	}
	u = strings.NewReplacer("<", "%3C", ">", "%3E").Replace(u)
	return "<" + u + ">"
}

// escapeBrackets escapes the brackets that would end link or image text.
func escapeBrackets(s string) string {
	return strings.NewReplacer("[", `\[`, "]", `\]`).Replace(s)
}

func (c *converter) list(n *html.Node, ordered bool) {
	gap := paragraph
	if len(c.lists) > 0 {
		gap = lineBreak
	}
	count := -1
	if ordered {
		count = 0
	}
	c.markerOnly = false
	c.block(gap)
	c.lists = append(c.lists, count)
	c.children(n)
	c.lists = c.lists[:len(c.lists)-1]
	c.markerOnly = false
	c.block(gap)
}

// item writes a list item: its marker, then its content indented under it.
func (c *converter) item(n *html.Node) {
	marker := bulletMarker
	if depth := len(c.lists) - 1; depth >= 0 && c.lists[depth] >= 0 {
		c.lists[depth]++
		marker = fmt.Sprintf("%d. ", c.lists[depth])
	}
	c.markerOnly = false
	c.block(lineBreak)
	c.open(marker)
	c.markerOnly = true
	pushed := c.pushPrefix(listIndent)
	c.children(n)
	c.popPrefix(pushed)
	c.markerOnly = false
	// Items stay on consecutive lines even when their content ends with a
	// paragraph.
	c.pending = min(c.pending, lineBreak)
	c.block(lineBreak)
}

// pre writes a fenced block with the original whitespace.
func (c *converter) pre(n *html.Node) {
	body := strings.TrimSuffix(rawText(n), "\n")
	fence := fenceFor(body)
	c.markerOnly = false
	c.block(paragraph)
	c.word(fence)
	linePrefix := c.linePrefix
	for line := range strings.SplitSeq(body, "\n") {
		if c.full {
			return
		}
		c.write("\n")
		c.write(linePrefix)
		c.write(line)
	}
	c.write("\n")
	c.write(linePrefix)
	c.write(fence)
	c.block(paragraph)
}

// fenceFor returns a backtick fence longer than any backtick run in body,
// and at least codeFence long.
func fenceFor(body string) string {
	longest, run := 0, 0
	for i := 0; i < len(body); i++ {
		if body[i] == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(len(codeFence), longest+1))
}

// code writes inline code, padding the delimiters when the code holds a
// backtick.
func (c *converter) code(n *html.Node) {
	s := collapse(rawText(n))
	if s == "" {
		return
	}
	if strings.Contains(s, "`") {
		s = "`` " + s + " ``"
	} else {
		s = "`" + s + "`"
	}
	c.word(s)
}

// table writes the rows of n as a markdown table, with a separator after
// the first row.
func (c *converter) table(n *html.Node) {
	rows := tableRows(n)
	if len(rows) == 0 {
		return
	}
	c.markerOnly = false
	c.block(paragraph)
	c.tables++
	defer func() { c.tables-- }()
	for i, row := range rows {
		// A row is written once all its cells are rendered, so each cell
		// gets only the room its siblings left. When the room runs out, the
		// cells rendered so far, the cut one included, are still written.
		cells := make([]string, 0, len(row))
		used := len("| ")
		cut := false
		for _, cell := range row {
			room := c.limit - len(c.buf) - used
			if room <= 0 {
				cut = true
				break
			}
			s, cellCut := c.cell(cell, room)
			if c.err != nil {
				return
			}
			cells = append(cells, s)
			used += len(s) + len(" | ")
			if cellCut {
				cut = true
				break
			}
		}
		if len(cells) > 0 {
			c.block(lineBreak)
			c.word("|")
			for _, s := range cells {
				c.write(" ")
				c.write(s)
				c.write(" |")
			}
		}
		if cut {
			c.full, c.truncated = true, true
		}
		if c.full {
			return
		}
		if i == 0 {
			c.block(lineBreak)
			c.word("|" + strings.Repeat(" --- |", len(row)))
		}
	}
	c.block(paragraph)
}

// cell renders a table cell on one line in at most room bytes before
// escaping, and reports whether it was cut at room. Only a cell of the
// outermost table escapes its column separators, so the separators of a
// table nested in it are escaped once. Cells of tables nested deeper than
// maxTableNesting are plain text.
func (c *converter) cell(n *html.Node, room int) (string, bool) {
	if c.tables > maxTableNesting {
		return collapse(rawText(n)), false
	}
	sub := c.sub(room)
	sub.children(n)
	c.adopt(sub)
	return collapseCell(sub.buf, c.tables == 1), sub.truncated
}

// collapseCell returns b on one line with its whitespace collapsed and,
// when escape is set, its column separators escaped.
func collapseCell(b []byte, escape bool) string {
	var sb strings.Builder
	sb.Grow(len(b))
	space := false
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		ch := b[:size] // as written, even when not valid UTF-8
		b = b[size:]
		if unicode.IsSpace(r) {
			space = sb.Len() > 0
			continue
		}
		if space {
			sb.WriteByte(' ')
			space = false
		}
		if escape && r == '|' {
			sb.WriteByte('\\')
		}
		sb.Write(ch)
	}
	return sb.String()
}

// tableRows returns the cells of the rows of table n, looking through
// thead, tbody and tfoot but not into nested tables.
func tableRows(n *html.Node) [][]*html.Node {
	var rows [][]*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type != html.ElementNode {
				continue
			}
			switch ch.Data {
			case "thead", "tbody", "tfoot":
				walk(ch)
			case "tr":
				var cells []*html.Node
				for td := ch.FirstChild; td != nil; td = td.NextSibling {
					if td.Type == html.ElementNode && (td.Data == "td" || td.Data == "th") {
						cells = append(cells, td)
					}
				}
				if len(cells) > 0 {
					rows = append(rows, cells)
				}
			}
		}
	}
	walk(n)
	return rows
}

// resolve returns ref resolved against the base URL, or ref unchanged when
// there is no base or ref does not parse.
func (c *converter) resolve(ref string) string {
	ref = strings.TrimSpace(ref)
	u, err := url.Parse(ref)
	if err != nil || c.base == nil {
		return ref
	}
	return c.base.ResolveReference(u).String()
}

// resolveBase returns the URL of a <base href>, resolved against base.
func resolveBase(base *url.URL, href string) *url.URL {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return base
	}
	if base == nil {
		return u
	}
	return base.ResolveReference(u)
}

func isJavaScript(href string) bool { return hasScheme(href, "javascript") }

// hasScheme reports whether ref is a URL of scheme, ignoring case and
// surrounding space.
func hasScheme(ref, scheme string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref)), scheme+":")
}

// find returns the first element named name outside dropped content other
// than head, or nil.
func find(n *html.Node, name string) *html.Node {
	if n.Type == html.ElementNode {
		if n.Data == name {
			return n
		}
		if droppedElements[n.Data] && n.Data != "head" {
			return nil
		}
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if el := find(ch, name); el != nil {
			return el
		}
	}
	return nil
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// rawText returns the text inside n as written, with <br> as a newline.
func rawText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			sb.WriteString(n.Data)
		case n.Type == html.ElementNode && n.Data == "br":
			sb.WriteByte('\n')
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return sb.String()
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
