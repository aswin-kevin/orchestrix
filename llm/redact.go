package llm

import (
	"net/url"
	"slices"
	"strings"
)

const (
	// redactedValue replaces each query value in a redacted URL.
	redactedValue = "REDACTED"
	// redactedSecret replaces a configured secret found in error text.
	redactedSecret = "[REDACTED]"
	// minSecretLen is the shortest secret redact replaces; a shorter one
	// could match ordinary words in an error body.
	minSecretLen = 8
)

// sensitiveHeaders are header names, lowercased, whose values are secrets.
// Any name containing one of sensitiveHeaderParts also counts.
var (
	sensitiveHeaders     = []string{"authorization", "proxy-authorization", "api-key", "x-api-key"}
	sensitiveHeaderParts = []string{"token", "secret", "key"}
)

// secrets returns the configured credentials that must not appear in an
// error: the API key, the values of sensitive Headers (and the credential
// after a scheme such as "Bearer "), and BaseURL's password and query
// values. Values shorter than minSecretLen are left out.
func (c *Config) secrets() []string {
	var out []string
	add := func(v string) {
		if len(v) >= minSecretLen {
			out = append(out, v)
		}
	}
	add(c.APIKey)
	for name, value := range c.Headers {
		if !isSensitiveHeader(name) {
			continue
		}
		add(value)
		if _, credential, ok := strings.Cut(value, " "); ok {
			add(strings.TrimSpace(credential))
		}
	}
	if u, err := url.Parse(c.BaseURL); err == nil {
		if password, ok := u.User.Password(); ok {
			add(password)
		}
		query, _ := url.ParseQuery(u.RawQuery)
		for _, values := range query {
			for _, v := range values {
				add(v)
			}
		}
	}
	// Longest first, so a secret containing another is replaced whole.
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

func isSensitiveHeader(name string) bool {
	name = strings.ToLower(name)
	if slices.Contains(sensitiveHeaders, name) {
		return true
	}
	for _, part := range sensitiveHeaderParts {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

// redact replaces every configured secret in s with [REDACTED], since some
// servers and proxies echo credentials in their errors.
func (c *Config) redact(s string) string {
	for _, secret := range c.secrets() {
		s = strings.ReplaceAll(s, secret, redactedSecret)
	}
	return s
}

// streamError is the error a provider reported inside a stream that started
// with a 200 (Status 0), with configured secrets redacted from its body.
func (c *Config) streamError(body string) *APIError {
	return &APIError{Body: c.redact(body)}
}

// redactedError carries a message with secrets redacted while still
// unwrapping to the original error, so errors.Is and errors.As see through
// it.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactError returns err unchanged when its message holds no configured
// secret, and otherwise wraps it with the redacted message.
func (c *Config) redactError(err error) error {
	msg := err.Error()
	if redacted := c.redact(msg); redacted != msg {
		return &redactedError{msg: redacted, err: err}
	}
	return err
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
