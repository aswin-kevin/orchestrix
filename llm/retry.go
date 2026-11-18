package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// maxErrorBodyBytes caps how much of an error response is kept.
	maxErrorBodyBytes = 64 << 10

	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
)

// do sends the request produced by build, honouring the rate limit and
// retrying transient failures, and returns the first 2xx response; the caller
// closes its body. build runs once per attempt so every retry gets a fresh
// request body. Retries cover only establishing the response, never reading
// it. Context errors are returned as-is and never retried.
func (c *Config) do(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	maxRetries := c.maxRetries()
	for attempt := 0; ; attempt++ {
		resp, err := c.attempt(ctx, build)
		if err == nil {
			return resp, nil
		}
		if attempt >= maxRetries || !IsRetryable(err) {
			return nil, err
		}
		if err := sleepContext(ctx, c.retryDelay(attempt, err)); err != nil {
			return nil, err
		}
	}
}

// attempt makes a single rate-limited request and converts a non-2xx reply
// into an *APIError.
func (c *Config) attempt(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	if err := c.limiter.acquire(ctx); err != nil {
		return nil, err
	}
	req, err := build()
	if err != nil {
		// %v, not %w: a malformed URL surfaces as *url.Error, which would
		// otherwise be mistaken for a retryable network failure.
		return nil, fmt.Errorf("llm: build request: %v", err)
	}
	resp, err := c.httpClient().Do(req.WithContext(ctx))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("llm: send request: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	// A failed read still leaves the status, which is what classification
	// needs, so the partial body is kept and the read error dropped.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return nil, &APIError{
		Status:        resp.StatusCode,
		Body:          string(body),
		RetryAfter:    retryAfter,
		hasRetryAfter: hasRetryAfter,
	}
}

func (c *Config) maxRetries() int {
	switch {
	case c.MaxRetries == 0:
		return DefaultMaxRetries
	case c.MaxRetries < 0:
		return 0
	}
	return c.MaxRetries
}

// retryDelay prefers the server's Retry-After, then the fixed interval, then
// exponential backoff.
func (c *Config) retryDelay(attempt int, err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.hasRetryAfter {
		return apiErr.RetryAfter
	}
	if c.RetryInterval > 0 {
		return c.RetryInterval
	}
	delay := initialBackoff
	for i := 0; i < attempt && delay < maxBackoff; i++ {
		delay *= 2
	}
	return min(delay, maxBackoff)
}
