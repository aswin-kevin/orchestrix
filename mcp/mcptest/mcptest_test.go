package mcptest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/mcp/mcptest"
)

// helperEnv makes the test binary serve mcptest.NewServer() over stdin and
// stdout instead of running tests.
const helperEnv = "ORX_MCPTEST_HELPER"

// brokenEnv makes a re-executed test binary run the kit against one mutant.
const brokenEnv = "ORX_MCPTEST_BROKEN"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		if err := mcptest.NewServer().ServeStdio(context.Background(), os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestInProcess(t *testing.T) {
	mcptest.Run(t, func(*testing.T) mcp.Transport { return mcp.InProcess(mcptest.NewServer()) })
}

func TestHTTP(t *testing.T) {
	mcptest.Run(t, func(t *testing.T) mcp.Transport {
		srv := httptest.NewServer(mcptest.NewServer().Handler(mcp.HandlerOptions{}))
		t.Cleanup(srv.Close)
		tr, err := mcp.DialHTTP(mcp.HTTPConfig{URL: srv.URL})
		if err != nil {
			t.Fatalf("DialHTTP: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close(context.Background()) })
		return tr
	})
}

func TestStdio(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stdio helper re-exec is exercised on unix")
	}
	mcptest.Run(t, func(t *testing.T) mcp.Transport {
		tr, err := mcp.StartStdio(mcp.StdioConfig{
			Command: os.Args[0],
			Args:    []string{"-test.run=^$"},
			// A race-enabled binary otherwise sleeps a second at exit.
			Env:        map[string]string{helperEnv: "1", "GORACE": "atexit_sleep_ms=0"},
			CloseGrace: 300 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("StartStdio: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close(context.Background()) })
		return tr
	})
}

// mutant wraps an in-process transport and breaks one behaviour. Incoming
// messages pass through transform, which returns what to deliver instead
// (possibly nothing, or several held-back messages).
type mutant struct {
	inner mcp.Transport
	out   chan json.RawMessage
	fwd   chan struct{} // closed when the forwarder has closed out

	transform func(json.RawMessage) []json.RawMessage
	// send, when set, replaces the forwarding of an outgoing message.
	send func(ctx context.Context, m *mutant, msg json.RawMessage) error
	// lateClose leaves Messages open for a while after Close returns.
	lateClose bool
	// err, when set, is what Err reports.
	err error
	// closeOnceOnly makes every Close after the first fail.
	closeOnceOnly bool

	closes atomic.Int32
	closed atomic.Bool
}

// newMutant starts a mutant over a fresh in-process session; configure
// adjusts it before its forwarder starts.
func newMutant(configure func(m *mutant)) *mutant {
	m := &mutant{
		inner: mcp.InProcess(mcptest.NewServer()),
		out:   make(chan json.RawMessage),
		fwd:   make(chan struct{}),
	}
	configure(m)
	go func() {
		defer close(m.fwd)
		for raw := range m.inner.Messages() {
			msgs := []json.RawMessage{raw}
			if m.transform != nil {
				msgs = m.transform(raw)
			}
			for _, msg := range msgs {
				m.out <- msg
			}
		}
		if m.lateClose {
			time.Sleep(500 * time.Millisecond)
		}
		close(m.out)
	}()
	return m
}

func (m *mutant) Send(ctx context.Context, msg json.RawMessage) error {
	if m.send != nil {
		return m.send(ctx, m, msg)
	}
	return m.inner.Send(ctx, msg)
}

func (m *mutant) Messages() <-chan json.RawMessage { return m.out }

func (m *mutant) Err() error {
	if m.err != nil && m.closed.Load() {
		return m.err
	}
	return m.inner.Err()
}

func (m *mutant) Close(ctx context.Context) error {
	if m.closes.Add(1) > 1 && m.closeOnceOnly {
		return errors.New("mutant: already closed")
	}
	m.closed.Store(true)
	err := m.inner.Close(ctx)
	if !m.lateClose {
		<-m.fwd
	}
	return err
}

// rewrite replaces from with to in every incoming message.
func rewrite(from, to string) func(json.RawMessage) []json.RawMessage {
	return func(raw json.RawMessage) []json.RawMessage {
		return []json.RawMessage{bytes.ReplaceAll(raw, []byte(from), []byte(to))}
	}
}

// staleResults answers every tools/call with the first tools/call result.
func staleResults() func(json.RawMessage) []json.RawMessage {
	var first json.RawMessage
	return func(raw json.RawMessage) []json.RawMessage {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Content json.RawMessage `json:"content"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Result.Content == nil {
			return []json.RawMessage{raw}
		}
		if first == nil {
			first = m.Result.Content
			return []json.RawMessage{raw}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"content": first}})
		return []json.RawMessage{out}
	}
}

// reversedNotifications holds notifications back and delivers them in
// reverse order just before the next response.
func reversedNotifications() func(json.RawMessage) []json.RawMessage {
	var held []json.RawMessage
	return func(raw json.RawMessage) []json.RawMessage {
		if bytes.Contains(raw, []byte(`"notifications/message"`)) {
			held = append(held, raw)
			return nil
		}
		slices.Reverse(held)
		out := append(held, raw)
		held = nil
		return out
	}
}

// clobberConcurrent overwrites the echo text of a message sent while
// another Send is in flight, like a transport sharing one write buffer.
func clobberConcurrent() func(ctx context.Context, m *mutant, msg json.RawMessage) error {
	var inFlight atomic.Int32
	return func(ctx context.Context, m *mutant, msg json.RawMessage) error {
		defer inFlight.Add(-1)
		if inFlight.Add(1) > 1 {
			msg = json.RawMessage(strings.Replace(string(msg), `"text":"`, `"text":"clobbered `, 1))
		}
		time.Sleep(2 * time.Millisecond) // widen the window
		return m.inner.Send(ctx, msg)
	}
}

// mutants maps "Case/how" to a configuration that breaks exactly the
// behaviour the kit case Case checks.
var mutants = map[string]func(m *mutant){
	"Handshake/wrongName":               func(m *mutant) { m.transform = rewrite(`"name":"mcptest"`, `"name":"other"`) },
	"Sequential/staleResults":           func(m *mutant) { m.transform = staleResults() },
	"Concurrent/clobberedSend":          func(m *mutant) { m.send = clobberConcurrent() },
	"NotificationsBeforeReply/reversed": func(m *mutant) { m.transform = reversedNotifications() },
	"CloseEndsMessages/stillOpen":       func(m *mutant) { m.lateClose = true },
	"CloseEndsMessages/errAfterClose":   func(m *mutant) { m.err = errors.New("mutant: closed") },
	"CloseIdempotent/secondCloseFails":  func(m *mutant) { m.closeOnceOnly = true },
	"SendAfterClose/silentDrop": func(m *mutant) {
		m.send = func(ctx context.Context, m *mutant, msg json.RawMessage) error {
			if m.closed.Load() {
				return nil
			}
			return m.inner.Send(ctx, msg)
		}
	},
}

// TestBrokenTransportsAreFlagged runs the kit in a child process against
// each mutant and checks that the case the mutant breaks fails.
func TestBrokenTransportsAreFlagged(t *testing.T) {
	if name := os.Getenv(brokenEnv); name != "" {
		mcptest.Run(t, func(t *testing.T) mcp.Transport {
			m := newMutant(mutants[name])
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = m.Close(ctx)
			})
			return m
		})
		return
	}
	if testing.Short() {
		t.Skip("re-executes the test binary per mutant")
	}
	for name := range mutants {
		wantFail, _, _ := strings.Cut(name, "/")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrokenTransportsAreFlagged$", "-test.v")
			cmd.Env = append(os.Environ(), brokenEnv+"="+name)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("kit passed a broken transport:\n%s", out)
			}
			if !strings.Contains(string(out), "--- FAIL: TestBrokenTransportsAreFlagged/"+wantFail+" (") {
				t.Fatalf("kit did not flag case %s:\n%s", wantFail, out)
			}
		})
	}
}

// TestMutantsArePassThroughOtherwise checks the mutant harness itself: an
// unconfigured mutant passes the whole kit, so a flagged case is due to the
// one behaviour each mutant breaks.
func TestMutantsArePassThroughOtherwise(t *testing.T) {
	mcptest.Run(t, func(t *testing.T) mcp.Transport {
		m := newMutant(func(*mutant) {})
		t.Cleanup(func() { _ = m.Close(context.Background()) })
		return m
	})
}
