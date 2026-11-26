package mcp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// pendingSessionTTL is how long a session may stay without
// notifications/initialized before newSession may evict it to make room.
const pendingSessionTTL = 30 * time.Second

// httpSession is one client's session of an httpHandler and the open
// responses its messages can go out on.
type httpSession struct {
	id              string
	sess            *session
	streamResponses bool
	now             func() time.Time // the handler's clock
	created         time.Time

	mu          sync.Mutex
	posts       map[string]*outStream // open POST exchanges by raw request id
	get         *outStream            // the open GET stream; nil when none
	open        int                   // open POST exchanges and GET streams
	lastActive  time.Time             // when the session was last used
	initialized bool                  // notifications/initialized arrived
}

// outMsg is a message on its way to the client. A response ends its POST
// exchange; failed marks an error response.
type outMsg struct {
	raw      json.RawMessage
	response bool
	failed   bool
}

// outStream carries messages to one open HTTP response.
type outStream struct {
	msgs chan outMsg
	gone chan struct{} // closed when the response is finished
}

func newOutStream() *outStream {
	return &outStream{msgs: make(chan outMsg), gone: make(chan struct{})}
}

// deliver hands m to the response, dropping it if the response is finished.
func (st *outStream) deliver(ctx context.Context, m outMsg) error {
	select {
	case st.msgs <- m:
		return nil
	case <-st.gone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newSession registers a session and names it in w's headers. It first
// removes idle sessions; when the table is still at its cap it also removes
// those without an open response still lacking notifications/initialized
// after pendingSessionTTL, so initialize requests that never complete the
// handshake cannot hold the slots. A client that skips the notification
// keeps its session while there is room. When the cap is still reached it
// writes 503 and returns nil.
func (h *httpHandler) newSession(w http.ResponseWriter) *httpSession {
	now := h.now()
	hs := &httpSession{
		id:              rand.Text(),
		streamResponses: !h.opts.JSONResponses,
		now:             h.now,
		created:         now,
		posts:           map[string]*outStream{},
		lastActive:      now,
	}

	h.mu.Lock()
	expired := h.removeExpiredLocked(now)
	if len(h.sessions) >= h.opts.MaxSessions {
		expired = append(expired, h.removePendingLocked(now)...)
	}
	full := len(h.sessions) >= h.opts.MaxSessions
	if !full {
		hs.sess = h.srv.newSession(context.Background(), hs.write)
		h.sessions[hs.id] = hs
	}
	h.mu.Unlock()
	for _, old := range expired {
		old.sess.close()
	}
	if full {
		http.Error(w, "too many sessions", http.StatusServiceUnavailable)
		return nil
	}
	w.Header().Set(headerSessionID, hs.id)
	return hs
}

// removeExpiredLocked unregisters and returns the sessions idle past the
// timeout; h.mu is held. The caller closes them after unlocking.
func (h *httpHandler) removeExpiredLocked(now time.Time) []*httpSession {
	var expired []*httpSession
	for id, hs := range h.sessions {
		if hs.idle(now, h.opts.SessionIdleTimeout) {
			delete(h.sessions, id)
			expired = append(expired, hs)
		}
	}
	return expired
}

// removePendingLocked unregisters and returns the sessions without an open
// response still lacking notifications/initialized pendingSessionTTL after
// they were created; h.mu is held. The caller closes them after unlocking.
func (h *httpHandler) removePendingLocked(now time.Time) []*httpSession {
	var pending []*httpSession
	for id, hs := range h.sessions {
		if hs.stalePending(now) {
			delete(h.sessions, id)
			pending = append(pending, hs)
		}
	}
	return pending
}

// lookup returns the session r names and marks it used, removing it when
// remove is set. Without an id it writes 400; for an unknown or expired id
// (an expired one is removed and closed) it writes 404. Either way it
// returns nil.
func (h *httpHandler) lookup(w http.ResponseWriter, r *http.Request, remove bool) *httpSession {
	id := r.Header.Get(headerSessionID)
	if id == "" {
		http.Error(w, "missing "+headerSessionID+" header", http.StatusBadRequest)
		return nil
	}
	h.mu.Lock()
	hs := h.sessions[id]
	expired := hs != nil && !hs.touch(h.now(), h.opts.SessionIdleTimeout)
	if hs != nil && (remove || expired) {
		delete(h.sessions, id)
	}
	h.mu.Unlock()
	if expired {
		hs.sess.close()
		hs = nil
	}
	if hs == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
	}
	return hs
}

// drop unregisters and ends a session, e.g. after its initialize failed.
func (h *httpHandler) drop(hs *httpSession) {
	h.mu.Lock()
	if h.sessions[hs.id] == hs {
		delete(h.sessions, hs.id)
	}
	h.mu.Unlock()
	hs.sess.close()
}

// idle reports whether the session has expired by now.
func (hs *httpSession) idle(now time.Time, timeout time.Duration) bool {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.expiredLocked(now, timeout)
}

// touch marks the session used at now, unless it had already expired, which
// it reports by returning false.
func (hs *httpSession) touch(now time.Time, timeout time.Duration) bool {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.expiredLocked(now, timeout) {
		return false
	}
	hs.lastActive = now
	return true
}

// expiredLocked reports whether the session has no open response and has
// had no use for timeout; hs.mu is held.
func (hs *httpSession) expiredLocked(now time.Time, timeout time.Duration) bool {
	return hs.open == 0 && now.Sub(hs.lastActive) >= timeout
}

// stalePending reports whether the session has no open response and has
// gone pendingSessionTTL since it was created without
// notifications/initialized.
func (hs *httpSession) stalePending(now time.Time) bool {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.open == 0 && !hs.initialized && now.Sub(hs.created) >= pendingSessionTTL
}

// markInitialized records that the client completed the handshake.
func (hs *httpSession) markInitialized() {
	hs.mu.Lock()
	hs.initialized = true
	hs.mu.Unlock()
}

// write routes a message from the session: a response to its request's
// POST exchange, a handler notification to its request's stream when
// responses are streamed, anything else to the GET stream. A message nobody
// is listening for is dropped.
func (hs *httpSession) write(ctx context.Context, raw json.RawMessage) error {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	out := outMsg{raw: raw, response: m.Method == "", failed: m.Error != nil}
	hs.mu.Lock()
	var st *outStream
	switch {
	case out.response:
		st = hs.posts[string(m.ID)]
	case hs.streamResponses:
		if id, ok := requestID(ctx); ok {
			st = hs.posts[id]
		}
	}
	if st == nil && !out.response {
		st = hs.get
	}
	hs.mu.Unlock()
	if st == nil {
		return nil
	}
	return st.deliver(ctx, out)
}

// openPost registers a POST exchange for request id, or returns nil when a
// request with that id is still in flight in the session.
func (hs *httpSession) openPost(id string) *outStream {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.posts[id] != nil || hs.sess.running(id) {
		return nil
	}
	st := newOutStream()
	hs.posts[id] = st
	hs.open++
	return st
}

// closePost finishes a POST exchange.
func (hs *httpSession) closePost(id string, st *outStream) {
	close(st.gone)
	hs.mu.Lock()
	delete(hs.posts, id)
	hs.open--
	hs.lastActive = hs.now()
	hs.mu.Unlock()
}

// openGet registers the session's GET stream, or returns nil when one is
// already open.
func (hs *httpSession) openGet() *outStream {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.get != nil {
		return nil
	}
	hs.get = newOutStream()
	hs.open++
	return hs.get
}

func (hs *httpSession) closeGet(st *outStream) {
	close(st.gone)
	hs.mu.Lock()
	hs.get = nil
	hs.open--
	hs.lastActive = hs.now()
	hs.mu.Unlock()
}
