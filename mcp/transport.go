package mcp

import (
	"context"
	"encoding/json"
)

// Transport moves JSON-RPC messages between a client and a server.
//
// Messages is closed when the transport ends, whether the other side went
// away or Close was called; Err then says why (nil after Close or when there
// is nothing more to say). Close is idempotent and releases everything the
// transport holds (process, connections, goroutines).
type Transport interface {
	// Send delivers one message, a single JSON object. It returns once the
	// message is handed to the other side, ctx ends, or the transport closes.
	Send(ctx context.Context, msg json.RawMessage) error
	// Messages yields every message received, in order.
	Messages() <-chan json.RawMessage
	// Err reports why the transport ended; it is meaningful once Messages is
	// closed.
	Err() error
	// Close ends the transport.
	Close() error
}

// protocolVersionSetter is implemented by transports that must know the
// negotiated protocol version (HTTP sends it as a header).
type protocolVersionSetter interface {
	SetProtocolVersion(version string)
}
