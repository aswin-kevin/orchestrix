package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// sessionKey is the context key under which a handler's ctx carries its
// session, for Notify.
type sessionKey struct{}

// session is one client's connection to a Server: it runs each request in
// its own goroutine and writes replies and notifications through write.
type session struct {
	srv   *Server
	write func(ctx context.Context, msg json.RawMessage) error

	ctx    context.Context // ends with the session; parent of every request
	cancel context.CancelFunc

	mu          sync.Mutex
	closed      bool
	initialized bool
	inflight    map[string]*request // by raw request id
	wg          sync.WaitGroup      // request and notification goroutines
}

// request is a running request; its identity tells a request from a later
// one that reuses its id.
type request struct {
	cancel context.CancelFunc
}

// newSession registers a session on s that ends when ctx does or close is
// called.
func (s *Server) newSession(ctx context.Context, write func(context.Context, json.RawMessage) error) *session {
	sess := &session{srv: s, write: write, inflight: map[string]*request{}}
	sess.ctx, sess.cancel = context.WithCancel(ctx)
	s.mu.Lock()
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()
	return sess
}

// close ends the session: it cancels every request and waits for their
// goroutines.
func (sess *session) close() {
	sess.mu.Lock()
	sess.closed = true
	sess.mu.Unlock()
	sess.srv.mu.Lock()
	delete(sess.srv.sessions, sess)
	sess.srv.mu.Unlock()
	sess.cancel()
	sess.wg.Wait()
}

// spawn runs f on a tracked goroutine unless the session is closed.
func (sess *session) spawn(f func()) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return false
	}
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()
		f()
	}()
	return true
}

// handle processes one incoming message.
func (sess *session) handle(raw json.RawMessage) error {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		reply := message{ID: json.RawMessage("null"), Error: &RPCError{Code: codeParseError, Message: "parse error"}}
		if !sess.spawn(func() { _ = sess.send(sess.ctx, encode(reply)) }) {
			return ErrClosed
		}
		return nil
	}
	switch {
	case m.isRequest():
		return sess.start(m)
	case m.Method == notifyCancelled:
		var p cancelledParams
		if json.Unmarshal(m.Params, &p) == nil {
			sess.cancelRequest(string(p.RequestID))
		}
	}
	return nil // other notifications and stray responses need nothing
}

// start registers a request and runs it on its own goroutine. Registering
// before handle returns means a notifications/cancelled that follows the
// request always finds it.
func (sess *session) start(m message) error {
	ctx, cancel := context.WithCancel(context.WithValue(sess.ctx, sessionKey{}, sess))
	id := string(m.ID)
	req := &request{cancel: cancel}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		cancel()
		return ErrClosed
	}
	sess.inflight[id] = req
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()
		defer cancel()
		defer sess.finish(id, req)
		sess.run(ctx, m)
	}()
	return nil
}

// finish forgets req unless a newer request has taken over its id.
func (sess *session) finish(id string, req *request) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.inflight[id] == req {
		delete(sess.inflight, id)
	}
}

// run answers one request unless it was cancelled first; a cancelled request
// gets no response.
func (sess *session) run(ctx context.Context, m message) {
	result, rpcErr := sess.dispatch(ctx, m)
	if ctx.Err() != nil {
		return
	}
	reply := message{ID: m.ID, Error: rpcErr}
	if rpcErr == nil {
		raw, err := marshalParams(result)
		if err != nil {
			reply.Error = &RPCError{Code: codeInternalError, Message: err.Error()}
		}
		reply.Result = raw
	}
	_ = sess.send(sess.ctx, encode(reply))
}

// send writes msg unless the session has ended.
func (sess *session) send(ctx context.Context, msg json.RawMessage) error {
	sess.mu.Lock()
	closed := sess.closed
	sess.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return sess.write(ctx, msg)
}

// dispatch runs the request, turning a handler panic into an internal error.
func (sess *session) dispatch(ctx context.Context, m message) (result any, rpcErr *RPCError) {
	defer func() {
		if r := recover(); r != nil {
			result, rpcErr = nil, &RPCError{Code: codeInternalError, Message: fmt.Sprintf("panic: %v", r)}
		}
	}()
	result, rpcErr = sess.srv.dispatch(ctx, m.Method, m.Params)
	if m.Method == methodInitialize && rpcErr == nil {
		sess.mu.Lock()
		sess.initialized = true
		sess.mu.Unlock()
	}
	return result, rpcErr
}

func (sess *session) cancelRequest(id string) {
	sess.mu.Lock()
	req := sess.inflight[id]
	sess.mu.Unlock()
	if req != nil {
		req.cancel()
	}
}

// notifyAsync sends a parameterless notification to an initialized session
// without blocking the caller.
func (sess *session) notifyAsync(method string) {
	sess.mu.Lock()
	ready := sess.initialized
	sess.mu.Unlock()
	if ready {
		sess.spawn(func() { _ = sess.send(sess.ctx, encode(message{Method: method})) })
	}
}

// Notify sends a notification to the client whose request ctx belongs to; it
// is for tool handlers and resource readers, e.g. to report progress. The
// notification is written before the request's response. Once the request
// has ended (finished, cancelled or its session closed) Notify returns an
// error and sends nothing.
func Notify(ctx context.Context, method string, params any) error {
	sess, ok := ctx.Value(sessionKey{}).(*session)
	if !ok {
		return errors.New("mcp: Notify outside a request handler")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mcp: Notify after the request ended: %w", err)
	}
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return sess.send(ctx, encode(message{Method: method, Params: raw}))
}
