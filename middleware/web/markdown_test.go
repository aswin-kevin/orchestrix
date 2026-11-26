package web

import (
	"context"
	"errors"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

func TestHTMLToMarkdown(t *testing.T) {
	tests := []struct {
		name, html, want string
	}{
		{"dropped elements",
			`<head><style>p{}</style><script>x()</script></head><body><script>evil()</script><noscript>no js</noscript><template><p>t</p></template><svg><title>icon</title><text>s</text></svg><iframe src="/f"></iframe><p>kept</p></body>`,
			"kept"},
		{"headings",
			`<h1>One</h1><h2>Two</h2><h3>Three</h3><h4>Four</h4><h5>Five</h5><h6>Six</h6>`,
			"# One\n\n## Two\n\n### Three\n\n#### Four\n\n##### Five\n\n###### Six"},
		{"paragraph breaks",
			`<p>a</p><div>b</div><section>c</section><article>d</article><header>e</header><footer>f</footer><main>g</main>`,
			"a\n\nb\n\nc\n\nd\n\ne\n\nf\n\ng"},
		{"blockquote",
			`<p>before</p><blockquote><p>one</p><p>two</p></blockquote><p>after</p>`,
			"before\n\n> one\n>\n> two\n\nafter"},
		{"br",
			`<p>line one<br>line two</p>`,
			"line one\nline two"},
		{"at most one blank line",
			`<p>a</p><br><br><br><p></p><div></div><p>b</p>`,
			"a\n\nb"},
		{"links",
			`<p>Read <a href="/docs/intro">the intro</a>, <a href="https://other.test/x">other</a> and <a href="#top">top</a>.</p>`,
			"Read [the intro](https://site.test/docs/intro), [other](https://other.test/x) and [top](https://site.test/dir/page#top)."},
		{"javascript link dropped to text",
			`<p><a href="javascript:alert(1)">click</a> <a href=" JavaScript:void(0)">here</a> <a>bare</a></p>`,
			"click here bare"},
		{"base href",
			`<html><head><base href="https://cdn.test/assets/"></head><body><a href="a.html">a</a> <img src="i.png" alt="pic"></body></html>`,
			"[a](https://cdn.test/assets/a.html) ![pic](https://cdn.test/assets/i.png)"},
		{"image",
			`<p><img src="logo.png" alt="The  logo"></p>`,
			"![The logo](https://site.test/dir/logo.png)"},
		{"link text brackets escaped",
			`<p><a href="/x">see [1] here</a></p>`,
			"[see \\[1\\] here](https://site.test/x)"},
		{"link URL with parens or spaces",
			`<p><a href="https://w.test/Go_(lang)">wiki</a> <img src="https://i.test/a b.png" alt="sp"></p>`,
			"[wiki](<https://w.test/Go_(lang)>) ![sp](https://i.test/a%20b.png)"},
		{"data image keeps alt text only",
			`<p>x <img src="data:image/png;base64,AAAA" alt="chart"> <img src=" DATA:image/gif;base64,R0" alt=""> y</p>`,
			"x chart y"},
		{"image alt brackets escaped",
			`<img src="/i.png" alt="a]b">`,
			"![a\\]b](https://site.test/i.png)"},
		{"unordered list",
			`<ul><li>one</li><li>two</li></ul>`,
			"- one\n- two"},
		{"ordered list",
			`<ol><li>first</li><li>second</li><li>third</li></ol>`,
			"1. first\n2. second\n3. third"},
		{"nested lists",
			`<ul><li>a<ul><li>a1</li><li>a2<ol><li>deep</li></ol></li></ul></li><li>b</li></ul>`,
			"- a\n  - a1\n  - a2\n    1. deep\n- b"},
		{"list item paragraphs",
			`<ul><li><p>para item</p></li><li>plain</li></ul>`,
			"- para item\n- plain"},
		{"list after text",
			`<p>Steps:</p><ol><li>go</li></ol><p>Done.</p>`,
			"Steps:\n\n1. go\n\nDone."},
		{"pre keeps whitespace",
			"<p>Run:</p><pre><code>func main() {\n\tfmt.Println(\"hi\")\n\n}</code></pre><p>end</p>",
			"Run:\n\n```\nfunc main() {\n\tfmt.Println(\"hi\")\n\n}\n```\n\nend"},
		{"pre with backtick fence inside",
			"<pre>```\ncode\n```</pre>",
			"````\n```\ncode\n```\n````"},
		{"inline code",
			`<p>Call <code>fmt.Println</code> or <code>a` + "`" + `b</code>.</p>`,
			"Call `fmt.Println` or `` a`b ``."},
		{"emphasis",
			`<p><strong>bold</strong>, <b>also</b>, <em>it</em> and <i>also it</i></p>`,
			"**bold**, **also**, *it* and *also it*"},
		{"emphasis spacing",
			`<p>a <b> spaced </b> word</p>`,
			"a **spaced** word"},
		{"table",
			`<table><thead><tr><th>Name</th><th>Kind</th></tr></thead><tbody><tr><td>go</td><td><a href="/go">lang</a></td></tr><tr><td>a|b</td><td></td></tr></tbody></table>`,
			"| Name | Kind |\n| --- | --- |\n| go | [lang](https://site.test/go) |\n| a\\|b |  |"},
		{"hr",
			`<p>a</p><hr><p>b</p>`,
			"a\n\n---\n\nb"},
		{"whitespace collapsed",
			"<p>  many \n\t spaces   here </p>\n\n\n<p>\n next </p>",
			"many spaces here\n\nnext"},
		{"entities decoded",
			`<p>&lt;tag&gt; &amp; &quot;q&quot; &copy; caf&eacute;&nbsp;bar</p>`,
			"<tag> & \"q\" © café bar"},
		{"inline elements flow",
			`<p>one <span>two</span>three</p>`,
			"one twothree"},
	}
	base, _ := url.Parse("https://site.test/dir/page")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got, _, err := HTMLToMarkdown(t.Context(), strings.NewReader(tt.html), base, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("markdown =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestHTMLToMarkdownTitle(t *testing.T) {
	title, md, _, err := HTMLToMarkdown(t.Context(), strings.NewReader(`<html><head><title> My
	Page </title></head><body><svg><title>icon</title></svg><p>body</p></body></html>`), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if title != "My Page" || md != "body" {
		t.Fatalf("title = %q, markdown = %q", title, md)
	}
}

func TestHTMLToMarkdownNilBase(t *testing.T) {
	_, md, _, err := HTMLToMarkdown(t.Context(), strings.NewReader(`<a href="/rel">r</a>`), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md != "[r](/rel)" {
		t.Fatalf("markdown = %q", md)
	}
}

func TestHTMLToMarkdownRawURLWithSpace(t *testing.T) {
	_, md, _, err := HTMLToMarkdown(t.Context(), strings.NewReader(`<a href="my page.html">x</a>`), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md != "[x](<my page.html>)" {
		t.Fatalf("markdown = %q", md)
	}
}

// convertWithin converts doc with HTMLToMarkdown, failing the test
// when it has not finished within d; a converter that ignores its bounds is
// then reported instead of hanging the test binary.
func convertWithin(t *testing.T, d time.Duration, ctx context.Context, doc string) (md string, truncated bool, err error) {
	t.Helper()
	type result struct {
		md        string
		truncated bool
		err       error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, md, truncated, err := HTMLToMarkdown(ctx, strings.NewReader(doc), nil, DefaultMaxMarkdownBytes)
		done <- result{md, truncated, err}
	}()
	select {
	case r := <-done:
		if elapsed := time.Since(start); elapsed > d {
			t.Fatalf("conversion took %s, want < %s", elapsed, d)
		}
		return r.md, r.truncated, r.err
	case <-time.After(10 * d):
		t.Fatalf("conversion still running after %s", 10*d)
		return "", false, nil
	}
}

func TestMarkdownNestedBlockquotesBounded(t *testing.T) {
	const depth = 500
	doc := strings.Repeat("<blockquote>", depth) + strings.Repeat("word ", 4<<20/5) + strings.Repeat("</blockquote>", depth)
	md, truncated, err := convertWithin(t, 2*time.Second, t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(md) > DefaultMaxMarkdownBytes {
		t.Fatalf("markdown is %d bytes, want at most %d", len(md), DefaultMaxMarkdownBytes)
	}
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if want := strings.Repeat(quotePrefix, maxNesting) + "word word"; !strings.HasPrefix(md, strings.TrimSpace(want)) {
		t.Fatalf("markdown starts %q, want the prefix stopped at %d quote markers", md[:min(len(md), 80)], maxNesting)
	}
}

func TestMarkdownNestedListsBounded(t *testing.T) {
	const depth = 200
	doc := strings.Repeat("<ul><li>x", depth) + strings.Repeat("</li></ul>", depth)
	md, _, err := convertWithin(t, 2*time.Second, t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(md, "\n") {
		if indent := len(line) - len(strings.TrimLeft(line, " ")); indent > maxNesting*len(listIndent) {
			t.Fatalf("line %q is indented %d spaces, want at most %d", line, indent, maxNesting*len(listIndent))
		}
	}
}

func TestMarkdownNestedTablesBounded(t *testing.T) {
	const depth = 120
	doc := strings.Repeat("<table><tr><td>a|b</td><td>", depth) + "c|d" + strings.Repeat("</td></tr></table>", depth)
	md, _, err := convertWithin(t, 2*time.Second, t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(md) > DefaultMaxMarkdownBytes {
		t.Fatalf("markdown is %d bytes, want at most %d", len(md), DefaultMaxMarkdownBytes)
	}
	if !strings.Contains(md, "c\\|d") || strings.Contains(md, `\\`) {
		t.Fatalf("markdown = %.300q, want the innermost cell escaped exactly once", md)
	}
}

func TestMarkdownTruncatedAtLimit(t *testing.T) {
	doc := "<p>" + strings.Repeat("é", 1000) + "</p>"
	_, md, truncated, err := HTMLToMarkdown(t.Context(), strings.NewReader(doc), nil, 101)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(md) > 101 || !utf8.ValidString(md) {
		t.Fatalf("markdown = %q (%d bytes), truncated = %v; want valid UTF-8 within 101 bytes, truncated", md, len(md), truncated)
	}
	_, md, truncated, err = HTMLToMarkdown(t.Context(), strings.NewReader("<p>short</p>"), nil, 101)
	if err != nil || truncated || md != "short" {
		t.Fatalf("markdown = %q, truncated = %v, err = %v", md, truncated, err)
	}
}

func TestFenceForLinear(t *testing.T) {
	start := time.Now()
	fence := fenceFor(strings.Repeat("`", 100000))
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("fenceFor took %s", elapsed)
	}
	if fence != strings.Repeat("`", 100001) {
		t.Fatalf("fence is %d bytes, want 100001 backticks", len(fence))
	}
	for body, want := range map[string]string{"": "```", "a`b``c": "```", "``` and `````": "``````"} {
		if got := fenceFor(body); got != want {
			t.Errorf("fenceFor(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestMarkdownHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, _, err := HTMLToMarkdown(ctx, strings.NewReader("<p>text</p>"), nil, DefaultMaxMarkdownBytes)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// cancelAfter is a context whose Err reports context.Canceled from its
// n-th call on.
type cancelAfter struct {
	context.Context
	n     int
	calls int
}

func (c *cancelAfter) Err() error {
	if c.calls++; c.calls >= c.n {
		return context.Canceled
	}
	return nil
}

func TestMarkdownCancelledMidway(t *testing.T) {
	ctx := &cancelAfter{Context: t.Context(), n: 2}
	doc := strings.Repeat("<p>a</p>", 10*checkEvery)
	_, md, _, err := HTMLToMarkdown(ctx, strings.NewReader(doc), nil, DefaultMaxMarkdownBytes)
	if !errors.Is(err, context.Canceled) || md != "" {
		t.Fatalf("markdown = %.40q, err = %v; want no markdown and context.Canceled", md, err)
	}
	if ctx.calls != 2 {
		t.Fatalf("context checked %d times, want conversion to stop at the first failing check", ctx.calls)
	}
}

// totalAlloc returns the bytes f allocates.
func totalAlloc(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestMarkdownWideTableRowBounded(t *testing.T) {
	// One row of cells, each rendering to far more markdown than its
	// HTML: the row is written only once all its cells are rendered, so
	// each cell must get only the room its siblings left.
	cell := "<td>" + strings.Repeat("<blockquote>", maxNesting) + strings.Repeat("<p>a</p>", 500) + "</td>"
	doc := "<table><tr>" + strings.Repeat(cell, 1200) + "</tr></table>"
	limit := len(doc)
	var md string
	var truncated bool
	var err error
	// The parse allocates a node per element whatever the converter does;
	// only what conversion allocates beyond it is bounded here.
	parse := totalAlloc(func() { _, _ = html.Parse(strings.NewReader(doc)) })
	all := totalAlloc(func() {
		_, md, truncated, err = HTMLToMarkdown(t.Context(), strings.NewReader(doc), nil, limit)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(md) > limit || !truncated {
		t.Fatalf("markdown is %d bytes, truncated = %v; want at most %d, truncated", len(md), truncated, limit)
	}
	// The cells' buffers and the output each grow by doubling to at most
	// limit, the cells are copied once collapsed, and the cut row is
	// written to the output, which is copied once more when returned:
	// about 9 × limit. Before cells shared the room of their row this was
	// over 100 × limit.
	t.Logf("parse allocated %d MB, conversion %d MB more", parse>>20, (int64(all)-int64(parse))>>20)
	if conv := int64(all) - int64(parse); conv > 12*int64(limit) {
		t.Fatalf("conversion allocated %d MB beyond the parse, want under %d MB", conv>>20, 12*limit>>20)
	}
}

func TestMarkdownNestedTableKeepsLinks(t *testing.T) {
	doc := `<table><tr><td>outer</td><td><table><tr><td><a href="https://example.com/in">inner link</a></td><td>x|y</td></tr></table></td></tr></table>`
	_, md, truncated, err := HTMLToMarkdown(t.Context(), strings.NewReader(doc), nil, DefaultMaxMarkdownBytes)
	if err != nil || truncated {
		t.Fatalf("err = %v, truncated = %v", err, truncated)
	}
	if !strings.Contains(md, "[inner link](https://example.com/in)") {
		t.Fatalf("markdown = %q, want the nested cell's link", md)
	}
	if !strings.Contains(md, `x\|y`) || strings.Contains(md, `\\`) {
		t.Fatalf("markdown = %q, want the nested cell's separator escaped exactly once", md)
	}
}

func TestMarkdownOverflowingRowKeepsCells(t *testing.T) {
	const limit = 200
	for name, doc := range map[string]string{
		"one cell":    "<table><tr><td>" + strings.Repeat("word ", 1000) + "</td></tr></table>",
		"second cell": "<table><tr><td>first</td><td>" + strings.Repeat("word ", 1000) + "</td></tr></table>",
	} {
		_, md, truncated, err := HTMLToMarkdown(t.Context(), strings.NewReader(doc), nil, limit)
		if err != nil {
			t.Fatal(err)
		}
		if md == "" || len(md) > limit || !truncated || !strings.Contains(md, "word word") {
			t.Errorf("%s: markdown = %q (%d bytes), truncated = %v; want the cut row within %d bytes, truncated", name, md, len(md), truncated, limit)
		}
		if name == "second cell" && !strings.HasPrefix(md, "| first | word") {
			t.Errorf("%s: markdown = %q, want it to start with the first cell", name, md)
		}
	}
}
