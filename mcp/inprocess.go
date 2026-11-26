package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
)

// inProcess is a Transport connected to a Server session in memory.
type inProcess struct {
	sess   *session
	msgs   chan json.RawMessage
	closed chan struct{}
	rel    release

	// msgsMu guards closing msgs: deliver holds it shared while sending,
	// Close exclusively while closing, so nothing sends on a closed channel.
	msgsMu     sync.RWMutex
	msgsClosed bool
}

// InProcess returns a Transport connected to a new session of s, without
// serialization through pipes or sockets beyond JSON encoding. It is meant
// for tests and for embedding a server in the same program.
//
// Close cancels the session's requests and waits for their handlers to
// return. A handler that ignores its ctx keeps the release running, so a
// Close whose ctx ends first returns ctx.Err() and the release goes on in
// the background.
func InProcess(s *Server) Transport {
	t := &inProcess{msgs: make(chan json.RawMessage), closed: make(chan struct{})}
	t.sess = s.newSession(context.Background(), t.deliver)
	return t
}

// deliver passes a server message to the client side.
func (t *inProcess) deliver(ctx context.Context, msg json.RawMessage) error {
	t.msgsMu.RLock()
	defer t.msgsMu.RUnlock()
	if t.msgsClosed {
		return ErrClosed
	}
	select {
	case t.msgs <- msg:
		return nil
	case <-t.closed:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send hands msg to the server session.
func (t *inProcess) Send(ctx context.Context, msg json.RawMessage) error {
	select {
	case <-t.closed:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return t.sess.handle(bytes.Clone(msg))
}

// Messages yields the server's messages.
func (t *inProcess) Messages() <-chan json.RawMessage { return t.msgs }

// Err is always nil: an in-process session only ends when closed.
func (t *inProcess) Err() error { return nil }

// Close ends the session, waiting for its requests' handlers to return or
// for ctx to end.
func (t *inProcess) Close(ctx context.Context) error {
	t.rel.start(func() error {
		close(t.closed) // unblocks deliver, which holds msgsMu shared
		t.sess.close()
		t.msgsMu.Lock()
		t.msgsClosed = true
		close(t.msgs)
		t.msgsMu.Unlock()
		return nil
	})
	return t.rel.wait(ctx)
}
