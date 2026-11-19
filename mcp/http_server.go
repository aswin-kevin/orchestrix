package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// Header and media type names of the streamable HTTP transport.
const (
	headerSessionID       = "Mcp-Session-Id"
	headerProtocolVersion = "MCP-Protocol-Version"
	mediaTypeJSON         = "application/json"
	mediaTypeEventStream  = "text/event-stream"
)

// sessionIDBytes is how many random bytes a session id holds; it is sent
// hex-encoded.
const sessionIDBytes = 16

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
}

// Handler serves s over the streamable HTTP transport, at whatever path it
// is mounted on. A POSTed initialize request creates a session whose random
// id is returned in the Mcp-Session-Id header; every other request must
// carry a known id (missing → 400, unknown → 404). POSTed notifications and
// responses get 202; requests are answered as JSON or, with
// StreamResponses, as an SSE stream. GET opens the session's notification
// stream, DELETE ends the session.
//
// Sessions live until DELETE: a client that never sends one, or whose
// initialize failed, leaves its session registered. Requests whose Origin header names another host are
// refused (403) against DNS rebinding; any other authentication is up to
// the embedding program.
func (s *Server) Handler(opts HandlerOptions) http.Handler {
	return &httpHandler{srv: s, opts: opts, sessions: map[string]*httpSession{}}
}

// httpHandler is the http.Handler returned by Server.Handler.
type httpHandler struct {
	srv  *Server
	opts HandlerOptions

	mu       sync.Mutex
	sessions map[string]*httpSession // by session id
}

// httpSession is one client's session and the open responses its messages
// can go out on.
type httpSession struct {
	sess            *session
	streamResponses bool

	mu    sync.Mutex
	posts map[string]*outStream // open POST exchanges by raw request id
	get   *outStream            // the open GET stream; nil when none
}

// outMsg is a message on its way to the client; a response ends its POST
// exchange.
type outMsg struct {
	raw      json.RawMessage
	response bool
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

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

// sameOrigin reports whether r has no Origin header or one naming the host
// it was sent to.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (h *httpHandler) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, DefaultMaxMessageBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("message over %d bytes", DefaultMaxMessageBytes), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil { // a batch (array) fails here too
		w.Header().Set("Content-Type", mediaTypeJSON)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(encode(message{ID: json.RawMessage("null"), Error: &RPCError{Code: codeParseError, Message: "parse error"}}))
		return
	}

	var hs *httpSession
	if m.isRequest() && m.Method == methodInitialize {
		hs = h.newSession(w)
	} else if hs = h.lookup(w, r, false); hs == nil {
		return
	}
	if !m.isRequest() {
		if err := hs.sess.handle(body); err != nil {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	h.answer(w, r, hs, string(m.ID), body)
}

// answer passes a request to the session and writes what it sends back for
// it: the response as JSON, or its notifications and response as SSE.
func (h *httpHandler) answer(w http.ResponseWriter, r *http.Request, hs *httpSession, id string, body []byte) {
	st := hs.openPost(id)
	defer hs.closePost(id, st)
	if err := hs.sess.handle(body); err != nil {
		http.NotFound(w, r)
		return
	}
	if h.opts.StreamResponses {
		startEventStream(w)
	}
	for {
		select {
		case m := <-st.msgs:
			if !h.opts.StreamResponses {
				w.Header().Set("Content-Type", mediaTypeJSON)
				_, _ = w.Write(m.raw)
				return
			}
			if writeEvent(w, m.raw) != nil || m.response {
				return
			}
		case <-r.Context().Done():
			return
		case <-hs.sess.ctx.Done():
			return
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
	startEventStream(w)
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

// newSession registers a session and names it in w's headers.
func (h *httpHandler) newSession(w http.ResponseWriter) *httpSession {
	buf := make([]byte, sessionIDBytes)
	_, _ = rand.Read(buf) // never fails
	id := hex.EncodeToString(buf)
	hs := &httpSession{streamResponses: h.opts.StreamResponses, posts: map[string]*outStream{}}
	hs.sess = h.srv.newSession(context.Background(), hs.write)
	h.mu.Lock()
	h.sessions[id] = hs
	h.mu.Unlock()
	w.Header().Set(headerSessionID, id)
	return hs
}

// lookup returns the session r names, removing it when remove is set, or
// writes 400 (no id) or 404 (unknown id) and returns nil.
func (h *httpHandler) lookup(w http.ResponseWriter, r *http.Request, remove bool) *httpSession {
	id := r.Header.Get(headerSessionID)
	if id == "" {
		http.Error(w, "missing "+headerSessionID+" header", http.StatusBadRequest)
		return nil
	}
	h.mu.Lock()
	hs := h.sessions[id]
	if remove {
		delete(h.sessions, id)
	}
	h.mu.Unlock()
	if hs == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
	}
	return hs
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
	out := outMsg{raw: raw, response: m.Method == ""}
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

func (hs *httpSession) openPost(id string) *outStream {
	st := newOutStream()
	hs.mu.Lock()
	hs.posts[id] = st
	hs.mu.Unlock()
	return st
}

// closePost finishes a POST exchange unless a newer one took over its id.
func (hs *httpSession) closePost(id string, st *outStream) {
	close(st.gone)
	hs.mu.Lock()
	if hs.posts[id] == st {
		delete(hs.posts, id)
	}
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
	return hs.get
}

func (hs *httpSession) closeGet(st *outStream) {
	close(st.gone)
	hs.mu.Lock()
	hs.get = nil
	hs.mu.Unlock()
}

// startEventStream sends the headers of an SSE response.
func startEventStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", mediaTypeEventStream)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
}

// writeEvent sends one message as an SSE "message" event and flushes it.
func writeEvent(w http.ResponseWriter, raw json.RawMessage) error {
	var b strings.Builder
	b.WriteString("event: message\n")
	for line := range strings.SplitSeq(string(raw), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}
