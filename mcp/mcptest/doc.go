// Package mcptest is a conformance kit for mcp.Transport implementations.
//
// Run checks a transport against a NewServer() server: the handshake,
// sequential and concurrent requests, notifications delivered in order
// before the response of the request that sent them, and the Close
// contract (Messages closed and Err nil once Close returns nil, Close
// idempotent, safe for concurrent use and bounded by its ctx, Send after
// Close failing with mcp.ErrClosed), both on a fresh transport and on one
// carrying a live session. Every blocking call runs under the case's ctx,
// so a call that never returns fails the case instead of hanging the test
// binary. Ignoring ctx is detected only where it shows: a Close whose ctx
// has already ended must return within 2 seconds; a Close or Send that
// ignores ctx but finishes within the case's 10 seconds otherwise passes.
package mcptest
