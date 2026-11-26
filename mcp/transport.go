package mcp

import (
	"context"
	"encoding/json"
	"sync"
)

// Transport moves JSON-RPC messages between a client and a server.
//
// Messages is closed when the transport ends, whether the other side went
// away or Close was called; Err then says why (nil after Close or when there
// is nothing more to say).
//
// Close releases everything the transport holds (process, connections,
// goroutines). It is idempotent and safe for concurrent use: the first call
// starts the release, and every call returns when the release is done (with
// its result) or with ctx.Err() when ctx ends first, the release going on
// in the background. When Close returns nil, Messages is already closed.
//
// ServerConfig stays flat: it carries only the common options of the stdio
// and HTTP transports. For anything else (HTTPConfig.Client,
// HTTPConfig.MaxMessageBytes, StdioConfig.CloseGrace and any later transport
// option), build the Transport yourself with DialHTTP or StartStdio and pass
// it as ServerConfig.Transport; Connect takes ownership of it.
type Transport interface {
	// Send delivers one message, a single JSON object. It returns once the
	// message is handed to the other side, ctx ends, or the transport closes.
	Send(ctx context.Context, msg json.RawMessage) error
	// Messages yields every message received, in order.
	Messages() <-chan json.RawMessage
	// Err reports why the transport ended; it is meaningful once Messages is
	// closed.
	Err() error
	// Close ends the transport; see the Transport documentation for its
	// contract.
	Close(ctx context.Context) error
}

// ProtocolVersionSetter is implemented by transports that must know the
// negotiated protocol version (HTTP sends it as a header). NewClient calls
// SetProtocolVersion once the handshake has settled on a version. A
// transport that wraps another must forward SetProtocolVersion to it, or the
// inner transport (such as HTTP's MCP-Protocol-Version header) never learns
// the negotiated version.
type ProtocolVersionSetter interface {
	// SetProtocolVersion records the negotiated protocol version.
	SetProtocolVersion(version string)
}

// release runs a one-time release in the background and lets any number of
// callers wait for it. It is shared by the transports and Client to
// implement the Close contract. The zero value is ready to use.
type release struct {
	once sync.Once
	done chan struct{}
	err  error // written before done is closed
}

// start runs fn in a goroutine on the first call; later calls do nothing.
func (r *release) start(fn func() error) {
	r.once.Do(func() {
		r.done = make(chan struct{})
		go func() {
			r.err = fn()
			close(r.done)
		}()
	})
}

// wait returns the release's result once it is done, or ctx.Err() when ctx
// ends first. It must be called after start.
func (r *release) wait(ctx context.Context) error {
	select {
	case <-r.done:
		return r.err
	default:
	}
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
