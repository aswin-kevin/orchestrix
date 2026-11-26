package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"
)

const (
	// maxErrorBodyBytes caps how much of an error response is kept.
	maxErrorBodyBytes = 64 << 10

	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second

	// retryAfterJitterDivisor bounds the jitter added to a server's
	// Retry-After: at most a tenth of the requested wait.
	retryAfterJitterDivisor = 10

	// maxRetryAfter caps the wait a server's Retry-After can ask for, so a
	// misbehaving server or proxy cannot stall a call indefinitely.
	maxRetryAfter = 60 * time.Second
)

// Do sends the request produced by build, honouring the rate limit and
// retrying transient failures, and returns the first 2xx response; the caller
// closes its body. build runs once per attempt so every retry gets a fresh
// request body. Retries cover only establishing the response, never reading
// it, and Config.StreamIdleTimeout does not apply (Stream and Fetch add it).
// Context errors are returned as-is and never retried. Other errors have
// configured credentials redacted, and a non-2xx reply becomes an *APIError.
//
// A retryable error is returned at once, without waiting, when a fallback
// provider will switch to another entry for it (see FallbackPending), or
// when the wait before the next attempt would end after ctx's deadline: the
// caller gets the provider's error while ctx is still live, rather than a
// deadline error once the wait has used up the time left.
func (t *Transport) Do(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	if t == nil || t.client == nil {
		return nil, errNoTransport
	}
	maxRetries := t.maxRetries()
	for attempt := 0; ; attempt++ {
		resp, err := t.attempt(ctx, build)
		if err == nil {
			return resp, nil
		}
		if attempt >= maxRetries || !IsRetryable(err) || FallbackPending(ctx, err) {
			return nil, err
		}
		delay := t.retryDelay(attempt, err)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return nil, err
		}
		if err := sleepContext(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// attempt makes a single rate-limited request and converts a non-2xx reply
// into an *APIError.
func (t *Transport) attempt(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	if err := t.limiter.acquire(ctx); err != nil {
		return nil, err
	}
	req, err := build()
	if err != nil {
		// %v, not %w: a malformed URL surfaces as *url.Error, which would
		// otherwise be mistaken for a retryable network failure. Its URL is
		// redacted because BaseURL may carry a key.
		return nil, t.Redact(fmt.Errorf("llm: build request: %v", redactURLError(err)))
	}
	resp, err := t.client.Do(req.WithContext(ctx))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// The URL may carry a key in its query or userinfo, and a proxy may
		// echo a credential in its error.
		return nil, t.Redact(fmt.Errorf("llm: send request: %w", redactURLError(err)))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	// A failed read still leaves the status, which is what classification
	// needs, so the partial body is kept and the read error dropped.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	header := resp.Header.Clone()
	header.Del("Set-Cookie")
	header.Del("Set-Cookie2")
	return nil, &APIError{
		Status:        resp.StatusCode,
		Body:          t.redact(string(body)),
		RequestID:     firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("request-id")),
		Header:        header,
		RetryAfter:    retryAfter,
		HasRetryAfter: hasRetryAfter,
	}
}

// maxRetries is the effective MaxRetries.
func (t *Transport) maxRetries() int {
	switch {
	case t.cfg.MaxRetries == 0:
		return DefaultMaxRetries
	case t.cfg.MaxRetries == NoRetries:
		return 0
	}
	return t.cfg.MaxRetries
}

// retryDelay prefers the server's Retry-After, then the fixed interval, then
// exponential backoff. The Retry-After and backoff delays are jittered so
// that callers sharing a rate limit, such as parallel subagents, spread their
// retries out instead of all retrying at the same moment. A Retry-After is
// only ever lengthened by the jitter, and is capped at maxRetryAfter first;
// a fixed interval is used as given.
func (t *Transport) retryDelay(attempt int, err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.HasRetryAfter {
		wait := min(apiErr.RetryAfter, maxRetryAfter)
		return wait + randomUpTo(wait/retryAfterJitterDivisor)
	}
	if t.cfg.RetryInterval > 0 {
		return t.cfg.RetryInterval
	}
	return equalJitter(backoff(attempt))
}

// backoff is the exponential backoff ceiling for a zero-based attempt.
func backoff(attempt int) time.Duration {
	delay := initialBackoff
	for i := 0; i < attempt && delay < maxBackoff; i++ {
		delay *= 2
	}
	return min(delay, maxBackoff)
}

// equalJitter returns a random delay between d/2 and d, keeping half of the
// backoff as a floor so retries still slow down as attempts mount.
func equalJitter(d time.Duration) time.Duration {
	half := d / 2
	return half + randomUpTo(d-half)
}

// randomUpTo returns a uniformly random duration in [0, d].
func randomUpTo(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d + 1)
}
