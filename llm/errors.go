package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// statusOverloaded is the non-standard status some providers return when
// they are temporarily out of capacity.
const statusOverloaded = 529

// maxErrorMessageBytes bounds how much of the body APIError.Error includes;
// the full text stays available in APIError.Body.
const maxErrorMessageBytes = 512

// overflowMarkers are lowercase fragments that providers put in the body of a
// 400/413 response when the prompt does not fit the model's context window.
var overflowMarkers = []string{
	"prompt is too long",
	"context_length_exceeded",
	"maximum context length",
	"context window",
	"too many tokens",
	"input is too long",
}

// transientStreamErrors are the error types and codes that mean a provider
// is out of capacity or failing on its side when it reports the error inside
// a stream that started with a 200.
var transientStreamErrors = map[string]bool{
	"overloaded_error":    true, // Anthropic
	"rate_limit_error":    true, // Anthropic
	"api_error":           true, // Anthropic
	"server_error":        true, // OpenAI chat and Responses
	"rate_limit_exceeded": true, // OpenAI Responses
}

// APIError is a non-2xx reply from a provider.
type APIError struct {
	// Status is the HTTP status code.
	Status int
	// Body is the (possibly truncated) response body.
	Body string
	// RetryAfter is the delay requested by the Retry-After header; see
	// hasRetryAfter to tell an explicit zero from an absent header.
	RetryAfter time.Duration

	// hasRetryAfter records that the header was present and valid, so that
	// "Retry-After: 0" (retry now) differs from no header at all.
	hasRetryAfter bool
}

// Error implements the error interface.
func (e *APIError) Error() string {
	body := e.Body
	if len(body) > maxErrorMessageBytes {
		body = body[:maxErrorMessageBytes] + "...(truncated)"
	}
	return fmt.Sprintf("llm: provider returned status %d: %s", e.Status, body)
}

// IsContextOverflow reports whether err says the prompt exceeded the model's
// context window.
func IsContextOverflow(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	// Status 0 marks an error reported inside an otherwise successful stream.
	switch apiErr.Status {
	case 0, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
	default:
		return false
	}
	body := strings.ToLower(apiErr.Body)
	for _, marker := range overflowMarkers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// isTransientStreamError reports whether err is an error reported inside a
// stream (an *APIError with Status 0) whose body names a transient error type
// or code (see transientStreamErrors). Only the "type" and "code" fields of
// the JSON body count, at any depth, so a message that merely mentions such
// a name does not.
func isTransientStreamError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 0 {
		return false
	}
	var body any
	if json.Unmarshal([]byte(apiErr.Body), &body) != nil {
		return false
	}
	return hasTransientType(body)
}

// hasTransientType walks a decoded JSON value looking for a "type" or "code"
// field that names a transient error.
func hasTransientType(v any) bool {
	switch v := v.(type) {
	case map[string]any:
		for k, field := range v {
			if s, ok := field.(string); ok && (k == "type" || k == "code") && transientStreamErrors[s] {
				return true
			}
			if hasTransientType(field) {
				return true
			}
		}
	case []any:
		for _, field := range v {
			if hasTransientType(field) {
				return true
			}
		}
	}
	return false
}

// IsRetryable reports whether repeating the request may succeed: transient
// HTTP statuses and network failures qualify, cancellation never does.
func IsRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return isRetryableStatus(apiErr.Status)
	}
	var urlErr *url.Error
	var netErr net.Error
	return errors.As(err, &urlErr) || errors.As(err, &netErr)
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests, statusOverloaded:
		return true
	}
	return status >= 500
}

// parseRetryAfter reads a Retry-After header holding either delay-seconds or
// an HTTP date. ok is false for a missing or malformed value; a date in the
// past yields a zero delay with ok true.
func parseRetryAfter(value string, now time.Time) (delay time.Duration, ok bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(when.Sub(now), 0), true
	}
	return 0, false
}
