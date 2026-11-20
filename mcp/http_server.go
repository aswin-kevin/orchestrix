package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Header and media type names of the streamable HTTP transport.
const (
	headerSessionID       = "Mcp-Session-Id"
	headerProtocolVersion = "MCP-Protocol-Version"
	mediaTypeJSON         = "application/json"
	mediaTypeEventStream  = "text/event-stream"
)

// Defaults of Server.Handler.
const (
	// DefaultMaxSessions caps the live sessions of one handler.
	DefaultMaxSessions = 1024
	// DefaultSessionIdleTimeout is how long a session may go without a
	// request or an open stream before it ends.
	DefaultSessionIdleTimeout = 30 * time.Minute
)

// eventWriteTimeout bounds writing one SSE event (or a stream's headers): a
// client that stops reading for this long loses the stream, which releases
// the goroutines waiting to send on it.
const eventWriteTimeout = 10 * time.Second

// HandlerOptions configure Server.Handler.
type HandlerOptions struct {
	// StreamResponses answers each POSTed request with an SSE stream: the
	// notifications its handler sends (see Notify), then its response.
	// false answers with a JSON body, and handler notifications go to the
	// session's GET stream if one is open.
	StreamResponses bool
	// DisableGET replies 405 to GET, so sessions get no server-initiated
	// stream and notifications such as tools/list_changed are dropped.
	// false lets each session keep one GET stream open.
	DisableGET bool
	// MaxSessions caps the live sessions; an initialize over the cap gets
	// 503. 0 → DefaultMaxSessions.
	MaxSessions int
	// SessionIdleTimeout ends a session that has had no request and no open
	// stream for this long; later requests for it get 404. 0 →
	// DefaultSessionIdleTimeout.
	SessionIdleTimeout time.Duration
	// AllowedHosts lists the Host header values the handler serves; any
	// other gets 403. An entry is a host name or IP literal ("localhost",
	// "127.0.0.1", "[::1]"), which matches with any port, or host:port,
	// which matches that port only; names compare case-insensitively, and
	// "*" allows every host. nil or empty → the loopback names localhost,
	// 127.0.0.1 and [::1] with any port, so a handler reachable under any
	// other name (a LAN address, a public name behind a proxy) must list it.
	AllowedHosts []string
}

// defaultAllowedHosts are the hosts a handler serves when
// HandlerOptions.AllowedHosts is empty.
var defaultAllowedHosts = []string{"localhost", "127.0.0.1", "[::1]"}

// Handler serves s over the streamable HTTP transport, at whatever path it
// is mounted on. A POSTed initialize request creates a session whose random
// id is returned in the Mcp-Session-Id header; every other request must
// carry a known id (missing → 400, unknown → 404). POSTed notifications and
// responses get 202; requests are answered as JSON or, with
// StreamResponses, as an SSE stream, and a request reusing the id of one
// still in flight gets 400. GET opens the session's notification stream,
// DELETE ends the session.
//
// A session ends on DELETE, when its initialize fails, after
// SessionIdleTimeout without requests or open streams, or when its client
// has not sent notifications/initialized within 30 seconds and no response
// is open. Expiry is lazy: an expired session is removed when it is next
// used or when a new session is created, so the handler runs no background
// goroutine and needs no Close.
// At most MaxSessions sessions are live; an initialize over the cap gets
// 503.
//
// Two checks run before anything else, each refusing with 403:
//   - The Host header must be in AllowedHosts (by default the loopback
//     names). This stops DNS rebinding: a page whose name is rebound to
//     127.0.0.1 still sends its own name as Host.
//   - A request with an Origin header must name the host it was sent to.
//     This stops a page on another origin from calling the server with a
//     cross-origin browser request. On its own it does not stop DNS
//     rebinding, where the page's origin and Host agree.
//
// Behind a reverse proxy, list the public name in AllowedHosts; if the
// proxy rewrites Host, browser requests fail the Origin check. Any other
// authentication is up to the embedding program.
func (s *Server) Handler(opts HandlerOptions) http.Handler {
	opts.MaxSessions = positiveOr(opts.MaxSessions, DefaultMaxSessions)
	opts.SessionIdleTimeout = positiveOr(opts.SessionIdleTimeout, DefaultSessionIdleTimeout)
	if len(opts.AllowedHosts) == 0 {
		opts.AllowedHosts = defaultAllowedHosts
	}
	return &httpHandler{srv: s, opts: opts, now: time.Now, sessions: map[string]*httpSession{}}
}

// httpHandler is the http.Handler returned by Server.Handler.
type httpHandler struct {
	srv  *Server
	opts HandlerOptions
	now  func() time.Time // the clock of session expiry

	mu       sync.Mutex
	sessions map[string]*httpSession // by session id
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !hostAllowed(r.Host, h.opts.AllowedHosts) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	if v := r.Header.Get(headerProtocolVersion); v != "" && !slices.Contains(SupportedVersions, v) {
		http.Error(w, "unsupported protocol version: "+v, http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPost:
		h.post(w, r)
	case http.MethodGet:
		h.get(w, r)
	case http.MethodDelete:
		h.delete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// hostAllowed reports whether host, a Host header, matches an entry of
// allowed (see HandlerOptions.AllowedHosts).
func hostAllowed(host string, allowed []string) bool {
	name := hostName(host)
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, host) {
			return true
		}
		if _, _, err := net.SplitHostPort(a); err != nil && strings.EqualFold(hostName(a), name) {
			return true // an entry without a port matches any port
		}
	}
	return false
}

// hostName returns hostport without its port and IPv6 brackets.
func hostName(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

// sameOrigin reports whether r has no Origin header or one naming the host
// it was sent to (hosts compare case-insensitively).
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (h *httpHandler) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, DefaultMaxMessageBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			msg := fmt.Sprintf("message over %d bytes", DefaultMaxMessageBytes)
			http.Error(w, msg, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil { // a batch (array) fails here too
		parseErr := &RPCError{Code: codeParseError, Message: "parse error"}
		w.Header().Set("Content-Type", mediaTypeJSON)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(encode(message{ID: json.RawMessage("null"), Error: parseErr}))
		return
	}

	initialize := m.isRequest() && m.Method == methodInitialize
	var hs *httpSession
	if initialize {
		hs = h.newSession(w)
	} else {
		hs = h.lookup(w, r, false)
	}
	if hs == nil {
		return
	}
	if !m.isRequest() {
		if err := hs.sess.handle(body); err != nil {
			http.NotFound(w, r)
			return
		}
		if m.Method == notifyInitialized {
			hs.markInitialized()
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if ok := h.answer(w, r, hs, string(m.ID), body); initialize && !ok {
		h.drop(hs)
	}
}

// answer passes a request to the session and writes what it sends back for
// it: the response as JSON, or its notifications and response as SSE. It
// reports whether a successful response was written.
func (h *httpHandler) answer(w http.ResponseWriter, r *http.Request, hs *httpSession, id string, body []byte) bool {
	st := hs.openPost(id)
	if st == nil {
		http.Error(w, "request id already in flight in this session", http.StatusBadRequest)
		return false
	}
	defer hs.closePost(id, st)
	if err := hs.sess.handle(body); err != nil {
		http.NotFound(w, r)
		return false
	}
	streaming := h.opts.StreamResponses
	if streaming && startEventStream(w) != nil {
		return false
	}
	for {
		select {
		case m := <-st.msgs:
			if !streaming {
				if m.failed {
					w.Header().Del(headerSessionID) // a failed initialize names no session
				}
				w.Header().Set("Content-Type", mediaTypeJSON)
				_, err := w.Write(m.raw)
				return err == nil && !m.failed
			}
			if err := writeEvent(w, m.raw); err != nil {
				return false
			}
			if m.response {
				return !m.failed
			}
		case <-r.Context().Done():
			return false
		case <-hs.sess.ctx.Done(): // ended by DELETE or expiry
			if !streaming {
				http.Error(w, "session ended", http.StatusNotFound)
			}
			return false
		}
	}
}

func (h *httpHandler) get(w http.ResponseWriter, r *http.Request) {
	if h.opts.DisableGET {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "no server-initiated stream", http.StatusMethodNotAllowed)
		return
	}
	hs := h.lookup(w, r, false)
	if hs == nil {
		return
	}
	st := hs.openGet()
	if st == nil {
		http.Error(w, "a stream is already open for this session", http.StatusConflict)
		return
	}
	defer hs.closeGet(st)
	if startEventStream(w) != nil {
		return
	}
	for {
		select {
		case m := <-st.msgs:
			if writeEvent(w, m.raw) != nil {
				return
			}
		case <-r.Context().Done():
			return
		case <-hs.sess.ctx.Done():
			return
		}
	}
}

func (h *httpHandler) delete(w http.ResponseWriter, r *http.Request) {
	hs := h.lookup(w, r, true)
	if hs == nil {
		return
	}
	hs.sess.close()
	w.WriteHeader(http.StatusOK)
}

// startEventStream sends the headers of an SSE response.
func startEventStream(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", mediaTypeEventStream)
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout)) // unsupported writers: no deadline
	w.WriteHeader(http.StatusOK)
	return rc.Flush()
}

// writeEvent sends one message as an SSE "message" event and flushes it,
// within eventWriteTimeout.
func writeEvent(w http.ResponseWriter, raw json.RawMessage) error {
	var b strings.Builder
	b.WriteString("event: message\n")
	for line := range strings.SplitSeq(string(raw), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout))
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	return rc.Flush()
}
