// Package web gives the agent the web: the web_fetch tool reads a page and
// returns it as markdown, and a SearchBackend can add web search.
//
// web_fetch reads pages through a Fetcher. The default, HTTPFetcher, guards
// against server-side request forgery: unless Config.AllowPrivateNetworks is
// set, connections to loopback, private, link-local and other non-public
// addresses are refused at connect time, for every redirect hop and every
// DNS answer. A custom Config.Fetcher owns its network policy instead. Pages
// are model input from an untrusted source; the default prompt says so.
package web
