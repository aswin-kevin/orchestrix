package web

import (
	"net/url"
	"strings"
	"testing"
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
			_, got, err := HTMLToMarkdown(strings.NewReader(tt.html), base)
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
	title, md, err := HTMLToMarkdown(strings.NewReader(`<html><head><title> My
	Page </title></head><body><svg><title>icon</title></svg><p>body</p></body></html>`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if title != "My Page" || md != "body" {
		t.Fatalf("title = %q, markdown = %q", title, md)
	}
}

func TestHTMLToMarkdownNilBase(t *testing.T) {
	_, md, err := HTMLToMarkdown(strings.NewReader(`<a href="/rel">r</a>`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if md != "[r](/rel)" {
		t.Fatalf("markdown = %q", md)
	}
}
