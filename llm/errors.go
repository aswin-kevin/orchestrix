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
// a stream that started with a 200, with the kind each one reports.
var transientStreamErrors = map[string]ErrorKind{
	"overloaded_error":    ErrorOverloaded, // Anthropic
	"rate_limit_error":    ErrorRateLimit,  // Anthropic
	"api_error":           ErrorServer,     // Anthropic
	"server_error":        ErrorServer,     // OpenAI chat and Responses
	"rate_limit_exceeded": ErrorRateLimit,  // OpenAI Responses
}

// ErrorKind classifies an APIError so callers can react to it without
// matching status codes or provider-specific bodies.
type ErrorKind string

// Kinds of APIError.
const (
	// ErrorRateLimit is a 429, or a rate-limit error inside a stream.
	ErrorRateLimit ErrorKind = "rate_limit"
	// ErrorAuth is a 401 or 403.
	ErrorAuth ErrorKind = "auth"
	// ErrorOverloaded is a 529 or 503, or an overloaded error inside a
	// stream.
	ErrorOverloaded ErrorKind = "overloaded"
	// ErrorInvalidRequest is a 400, 404 or 422 that is not a context
	// overflow.
	ErrorInvalidRequest ErrorKind = "invalid_request"
	// ErrorContextOverflow means the prompt exceeded the model's context
	// window; see IsContextOverflow.
	ErrorContextOverflow ErrorKind = "context_overflow"
	// ErrorServer is any other 5xx, or a server error inside a stream.
	ErrorServer ErrorKind = "server"
	// ErrorUnknown is anything else.
	ErrorUnknown ErrorKind = "unknown"
)

// APIError is a non-2xx reply from a provider, or, with Status 0, an error
// the provider reported inside a stream that started with a 200.
type APIError struct {
	// Status is the HTTP status code.
	Status int
	// Body is the (possibly truncated) response body, with any echo of a
	// configured credential (API key, sensitive header, BaseURL password or
	// query value) replaced by [REDACTED].
	Body string
	// RequestID is the provider's id for the request, from the x-request-id
	// or request-id header, for quoting to the provider's support.
	RequestID string
	// Header is the response header minus Set-Cookie and Set-Cookie2; nil
	// for an error inside a stream.
	Header http.Header
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
	if e.RequestID != "" {
		return fmt.Sprintf("llm: provider returned status %d (request id %s): %s", e.Status, e.RequestID, body)
	}
	return fmt.Sprintf("llm: provider returned status %d: %s", e.Status, body)
}

// Kind classifies the error. A context overflow takes precedence over the
// invalid request its status would otherwise make it.
func (e *APIError) Kind() ErrorKind {
	if IsContextOverflow(e) {
		return ErrorContextOverflow
	}
	switch e.Status {
	case 0:
		if kind := transientStreamKind(e); kind != "" {
			return kind
		}
		return ErrorUnknown
	case http.StatusTooManyRequests:
		return ErrorRateLimit
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrorAuth
	case statusOverloaded, http.StatusServiceUnavailable:
		return ErrorOverloaded
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return ErrorInvalidRequest
	}
	if e.Status >= 500 {
		return ErrorServer
	}
	return ErrorUnknown
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
	return errors.As(err, &apiErr) && transientStreamKind(apiErr) != ""
}

// transientStreamKind returns the kind of the transient error named by a
// stream error's body, or "" when e is not a stream error (Status 0) or
// names none.
func transientStreamKind(e *APIError) ErrorKind {
	if e.Status != 0 {
		return ""
	}
	var body any
	if json.Unmarshal([]byte(e.Body), &body) != nil {
		return ""
	}
	return transientType(body)
}

// transientType walks a decoded JSON value looking for a "type" or "code"
// field that names a transient error, and returns that error's kind.
func transientType(v any) ErrorKind {
	switch v := v.(type) {
	case map[string]any:
		for k, field := range v {
			if s, ok := field.(string); ok && (k == "type" || k == "code") {
				if kind := transientStreamErrors[s]; kind != "" {
					return kind
				}
			}
			if kind := transientType(field); kind != "" {
				return kind
			}
		}
	case []any:
		for _, field := range v {
			if kind := transientType(field); kind != "" {
				return kind
			}
		}
	}
	return ""
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
