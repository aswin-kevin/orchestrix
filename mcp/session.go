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

// requestIDKey is the context key under which a handler's ctx carries its
// request's raw id, so a transport can route the handler's notifications to
// that request's stream.
type requestIDKey struct{}

// requestID returns the raw id of the request whose handler ctx belongs to.
func requestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey{}).(string)
	return id, ok
}

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
func (s *session) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.srv.mu.Lock()
	delete(s.srv.sessions, s)
	s.srv.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

// spawn runs f on a tracked goroutine unless the session is closed.
func (s *session) spawn(f func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
	return true
}

// handle processes one incoming message.
func (s *session) handle(raw json.RawMessage) error {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		reply := message{ID: json.RawMessage("null"), Error: &RPCError{Code: codeParseError, Message: "parse error"}}
		if !s.spawn(func() { _ = s.send(s.ctx, encode(reply)) }) {
			return ErrClosed
		}
		return nil
	}
	switch {
	case m.isRequest():
		return s.start(m)
	case m.Method == notifyCancelled:
		var p cancelledParams
		if json.Unmarshal(m.Params, &p) == nil {
			s.cancelRequest(string(p.RequestID))
		}
	}
	return nil // other notifications and stray responses need nothing
}

// start registers a request and runs it on its own goroutine. Registering
// before handle returns means a notifications/cancelled that follows the
// request always finds it.
func (s *session) start(m message) error {
	id := string(m.ID)
	ctx := context.WithValue(context.WithValue(s.ctx, sessionKey{}, s), requestIDKey{}, id)
	ctx, cancel := context.WithCancel(ctx)
	req := &request{cancel: cancel}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		cancel()
		return ErrClosed
	}
	s.inflight[id] = req
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer s.finish(id, req)
		s.run(ctx, m)
	}()
	return nil
}

// finish forgets req unless a newer request has taken over its id.
func (s *session) finish(id string, req *request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[id] == req {
		delete(s.inflight, id)
	}
}

// run answers one request unless it was cancelled first; a cancelled request
// gets no response.
func (s *session) run(ctx context.Context, m message) {
	result, rpcErr := s.dispatch(ctx, m)
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
	_ = s.send(s.ctx, encode(reply))
}

// send writes msg unless the session has ended.
func (s *session) send(ctx context.Context, msg json.RawMessage) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return s.write(ctx, msg)
}

// dispatch runs the request, turning a handler panic into an internal error.
func (s *session) dispatch(ctx context.Context, m message) (result any, rpcErr *RPCError) {
	defer func() {
		if r := recover(); r != nil {
			result, rpcErr = nil, &RPCError{Code: codeInternalError, Message: fmt.Sprintf("panic: %v", r)}
		}
	}()
	result, rpcErr = s.srv.dispatch(ctx, m.Method, m.Params)
	if m.Method == methodInitialize && rpcErr == nil {
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
	}
	return result, rpcErr
}

// running reports whether a request with raw id is in flight.
func (s *session) running(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[id] != nil
}

func (s *session) cancelRequest(id string) {
	s.mu.Lock()
	req := s.inflight[id]
	s.mu.Unlock()
	if req != nil {
		req.cancel()
	}
}

// notifyAsync sends a parameterless notification to an initialized session
// without blocking the caller.
func (s *session) notifyAsync(method string) {
	s.mu.Lock()
	ready := s.initialized
	s.mu.Unlock()
	if ready {
		s.spawn(func() { _ = s.send(s.ctx, encode(message{Method: method})) })
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
