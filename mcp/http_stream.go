package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/internal/sse"
)

// relay pushes the data of every message event of an SSE body. It reports
// whether any event arrived and the error that ended the stream (nil at EOF
// or when the transport ended).
func (t *httpTransport) relay(body io.Reader) (received bool, err error) {
	for ev, err := range sse.Events(body, t.max) {
		if err != nil {
			return received, err
		}
		received = true
		if (ev.Event != "" && ev.Event != "message") || strings.TrimSpace(ev.Data) == "" {
			continue // not a JSON-RPC message (e.g. a priming event)
		}
		if !t.push(json.RawMessage(ev.Data)) {
			return received, nil
		}
	}
	return received, nil
}

// startListener starts the GET listener once.
func (t *httpTransport) startListener() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.listening || t.ended {
		return
	}
	t.listening = true
	t.wg.Go(func() {
		t.listen()
	})
}

// listen keeps a GET stream open, reopening it after t.retry, until the
// transport ends, the server refuses the stream, or DefaultMaxSSERetries
// attempts in a row fail. A stream that delivered an event or stayed open
// for at least t.retry was healthy, so its end (a server or proxy dropping
// an idle stream, say) is not a failure.
func (t *httpTransport) listen() {
	failures := 0
	for {
		healthy, stop := t.listenOnce()
		if stop || t.ctx.Err() != nil {
			return
		}
		if healthy {
			failures = 0
		} else if failures++; failures >= DefaultMaxSSERetries {
			return
		}
		timer := time.NewTimer(t.retry)
		select {
		case <-timer.C:
		case <-t.ctx.Done():
			timer.Stop()
			return
		}
	}
}

// listenOnce runs one GET stream to its end. It reports whether the stream
// was healthy (delivered an event or stayed open for at least t.retry) and
// whether listening should stop for good (405, an expired session, an
// oversized event).
func (t *httpTransport) listenOnce() (healthy, stop bool) {
	req, sid, err := t.newRequest(t.ctx, http.MethodGet, nil)
	if err != nil {
		return false, true
	}
	req.Header.Set("Accept", mediaTypeEventStream)
	resp, err := t.client.Do(req)
	if err != nil {
		return false, false
	}
	defer drainClose(resp.Body)
	switch {
	case resp.StatusCode == http.StatusMethodNotAllowed:
		return false, true
	case resp.StatusCode == http.StatusNotFound && sid != "":
		t.end(ErrSessionExpired)
		return false, true
	case resp.StatusCode != http.StatusOK || !hasMediaType(resp, mediaTypeEventStream):
		return false, false
	}
	opened := time.Now()
	received, err := t.relay(resp.Body)
	if errors.Is(err, sse.ErrTooLarge) {
		t.end(fmt.Errorf("mcp: http: %w", err))
		return received, true
	}
	return received || time.Since(opened) >= t.retry, false
}
