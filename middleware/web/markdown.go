package web

import (
	"fmt"
	"io"
	"net/url"
	"strings"

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

// HTMLToMarkdown converts an HTML document to markdown and returns its
// title (whitespace collapsed; empty when it has none). Relative links and
// image sources are resolved against the document's <base href>, itself
// resolved against base; a nil base leaves them as they are written.
// Scripts, styles and other non-content elements are dropped, and links to
// javascript: URLs keep only their text. It fails only on documents the
// parser rejects, such as ones nested more than 512 elements deep; the
// parser's bound on nesting also bounds the conversion's recursion.
func HTMLToMarkdown(r io.Reader, base *url.URL) (title, markdown string, err error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", "", err
	}
	if el := find(doc, "title"); el != nil {
		title = collapse(rawText(el))
	}
	if el := find(doc, "base"); el != nil {
		if href, ok := attr(el, "href"); ok {
			base = resolveBase(base, href)
		}
	}
	c := newConverter(base)
	c.children(doc)
	return title, c.String(), nil
}

func (c *converter) children(n *html.Node) {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.node(ch)
	}
}

func (c *converter) node(n *html.Node) {
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
	switch n.Data {
	case "blockquote":
		c.block(paragraph)
		c.prefix = append(c.prefix, quotePrefix)
		c.children(n)
		c.prefix = c.prefix[:len(c.prefix)-1]
		c.block(paragraph)
	case "br":
		c.br()
	case "hr":
		c.block(paragraph)
		c.word("---")
		c.block(paragraph)
	case "a":
		c.link(n)
	case "img":
		c.image(n)
	case "ul", "ol":
		c.list(n, n.Data == "ol")
	case "li":
		c.item(n)
	case "pre":
		c.pre(n)
	case "code":
		c.code(n)
	case "strong", "b":
		c.wrap(n, "**")
	case "em", "i":
		c.wrap(n, "*")
	case "table":
		c.table(n)
	default:
		c.children(n)
	}
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
	c.prefix = append(c.prefix, listIndent)
	c.children(n)
	c.prefix = c.prefix[:len(c.prefix)-1]
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
	linePrefix := strings.Join(c.prefix, "")
	for _, line := range strings.Split(body, "\n") {
		c.buf = append(c.buf, '\n')
		c.buf = append(c.buf, linePrefix...)
		c.buf = append(c.buf, line...)
	}
	c.buf = append(c.buf, '\n')
	c.buf = append(c.buf, linePrefix...)
	c.buf = append(c.buf, fence...)
	c.block(paragraph)
}

// fenceFor returns a backtick fence longer than any backtick run in body.
func fenceFor(body string) string {
	fence := codeFence
	for strings.Contains(body, fence) {
		fence += "`"
	}
	return fence
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
	for i, row := range rows {
		cells := make([]string, len(row))
		for j, cell := range row {
			cells[j] = c.cell(cell)
		}
		c.block(lineBreak)
		c.word("| " + strings.Join(cells, " | ") + " |")
		if i == 0 {
			c.block(lineBreak)
			c.word("|" + strings.Repeat(" --- |", len(row)))
		}
	}
	c.block(paragraph)
}

// cell renders a table cell on one line, escaping the column separator.
func (c *converter) cell(n *html.Node) string {
	sub := newConverter(c.base)
	sub.children(n)
	s := strings.Join(strings.Fields(sub.String()), " ")
	return strings.ReplaceAll(s, "|", `\|`)
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
