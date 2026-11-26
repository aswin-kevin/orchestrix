// Package mcp speaks the Model Context Protocol (revision 2025-06-18): a
// JSON-RPC 2.0 client for tools and resources over a pluggable Transport, a
// stdio transport that runs a server as a child process, a streamable HTTP
// transport, and a small Server that can be served over stdio, HTTP or in
// process. OAuth, sampling and elicitation are not supported.
//
// Interfaces never gain methods after v1; capabilities are optional
// interfaces discovered by type assertion.
package mcp
