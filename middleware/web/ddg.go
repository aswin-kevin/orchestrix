package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

const defaultDuckDuckGoURL = "https://html.duckduckgo.com/html/"

// DuckDuckGoConfig configures the DuckDuckGo backend. It needs no key.
type DuckDuckGoConfig struct {
	// BaseURL is the search endpoint; "" means
	// "https://html.duckduckgo.com/html/".
	BaseURL string
	// Client sends the requests. Nil builds a client without the
	// private-address policy of web_fetch, since the endpoint is fixed by
	// the developer and not chosen by the model. To search through a proxy
	// or with extra root certificates, pass a client from NewHTTPClient:
	//
	//	c, err := web.NewHTTPClient(web.ClientConfig{ProxyURL: "http://proxy:3128", AllowPrivateNetworks: true})
	//	...
	//	web.NewDuckDuckGo(web.DuckDuckGoConfig{Client: c})
	Client *http.Client
	// UserAgent is sent with every request; "" means DefaultUserAgent.
	UserAgent string
}

// DuckDuckGo is a SearchBackend that searches the HTML endpoint of
// DuckDuckGo. It is safe for concurrent use.
//
// DuckDuckGo answers automated clients it distrusts with a bot-challenge
// page. A challenge answered with status 202 is an error; one answered with
// 200 has no results, so it comes back as no results.
type DuckDuckGo struct {
	sc *searchClient
}

// NewDuckDuckGo returns a DuckDuckGo backend. It fails when the default
// client cannot be built.
func NewDuckDuckGo(cfg DuckDuckGoConfig) (*DuckDuckGo, error) {
	sc, err := newSearchClient("duckduckgo", cfg.Client, cfg.UserAgent, cfg.BaseURL, defaultDuckDuckGoURL)
	if err != nil {
		return nil, err
	}
	return &DuckDuckGo{sc: sc}, nil
}

// Search implements SearchBackend.
func (d *DuckDuckGo) Search(ctx context.Context, query string, n int) ([]SearchResult, error) {
	if n <= 0 {
		return nil, nil
	}
	form := url.Values{"q": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.sc.baseURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, status, err := d.sc.do(req)
	if err != nil {
		return nil, err
	}
	if status == http.StatusAccepted {
		return nil, errors.New("duckduckgo: HTTP 202 (challenge)")
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var results []SearchResult
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "div" && hasClass(node, "result") && !hasClass(node, "result--ad") {
			if r, ok := ddgResult(node); ok {
				results = append(results, r)
			}
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return clean(results, n), nil
}

// ddgResult reads the title, link and snippet of one result div.
func ddgResult(div *html.Node) (SearchResult, bool) {
	link := findClass(div, "result__a")
	if link == nil {
		return SearchResult{}, false
	}
	r := SearchResult{Title: nodeText(link), URL: ddgTarget(hrefOf(link))}
	if snip := findClass(div, "result__snippet"); snip != nil {
		r.Snippet = nodeText(snip)
	}
	return r, true
}

func hrefOf(n *html.Node) string {
	href, _ := attr(n, "href")
	return href
}

// ddgBase resolves the relative and protocol-relative links of result pages.
var ddgBase = &url.URL{Scheme: "https", Host: "duckduckgo.com", Path: "/l/"}

// ddgTarget returns the page a result link leads to: the uddg parameter of
// DuckDuckGo's redirect links, or the link itself with a missing scheme
// taken as https. It returns "" for an unparsable link.
func ddgTarget(href string) string {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return ""
	}
	u = ddgBase.ResolveReference(u)
	if u.Hostname() == ddgBase.Host && u.Path == ddgBase.Path {
		// A redirect link: its target, or nothing when it names none.
		return u.Query().Get("uddg")
	}
	return u.String()
}

func hasClass(n *html.Node, class string) bool {
	classes, _ := attr(n, "class")
	return slices.Contains(strings.Fields(classes), class)
}

// findClass returns the first descendant of n with class, or nil.
func findClass(n *html.Node, class string) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && hasClass(c, class) {
			return c
		}
		if found := findClass(c, class); found != nil {
			return found
		}
	}
	return nil
}

// nodeText returns the text under n with runs of whitespace collapsed.
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return collapse(b.String())
}
