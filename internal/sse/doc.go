// Package sse reads server-sent event streams (text/event-stream bodies) with
// a cap on the size of one event, so a misbehaving server cannot make a
// reader buffer without bound. It is shared by the model providers and the
// MCP HTTP transport.
package sse
