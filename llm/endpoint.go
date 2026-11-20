package llm

import (
	"fmt"
	"net/url"
	"strings"
)

// endpoint joins path onto the path of base, keeping base's query, such as a
// key some gateways take as a query parameter, after the joined path.
func endpoint(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		// The parse error quotes the URL, credentials included.
		return "", fmt.Errorf("llm: invalid BaseURL %q", redactURL(base))
	}
	return u.JoinPath(path).String(), nil
}

// versionedEndpoint is endpoint for APIs served under /v1, where BaseURL may
// or may not already end with /v1.
func versionedEndpoint(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("llm: invalid BaseURL %q", redactURL(base))
	}
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
		path = "v1/" + path
	}
	return u.JoinPath(path).String(), nil
}
