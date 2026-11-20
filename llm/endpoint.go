package llm

import (
	"fmt"
	"net/url"
	"strings"
)

// redactedValue replaces each query value in a redacted URL.
const redactedValue = "REDACTED"

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

// redactURL returns s without its userinfo and with every query value
// replaced by REDACTED, so it can appear in an error. A URL that does not
// parse is replaced whole, since its credentials cannot be located.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "[unparsable URL]"
	}
	u.User = nil
	if u.RawQuery != "" {
		// Pairs that do not parse are dropped, which keeps them out too.
		query, _ := url.ParseQuery(u.RawQuery)
		for key, values := range query {
			for i := range values {
				values[i] = redactedValue
			}
			query[key] = values
		}
		u.RawQuery = query.Encode()
	}
	return u.String()
}

// redactURLError returns err with the URL of a *url.Error redacted. The
// wrapped error is kept, so errors.Is and errors.As still see through it.
func redactURLError(err error) error {
	urlErr, ok := err.(*url.Error)
	if !ok {
		return err
	}
	return &url.Error{Op: urlErr.Op, URL: redactURL(urlErr.URL), Err: urlErr.Err}
}
