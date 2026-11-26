package mcptest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
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
		// Streamed responses carry each request's notifications before its
		// response on the same stream; see Run.
		srv := httptest.NewServer(mcptest.NewServer().Handler(mcp.HandlerOptions{StreamResponses: true}))
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
// (possibly nothing, or several held-back messages; a nil entry is a
// pause of notifyLag). The harness itself never hangs: the forwarder stops
// delivering once Close starts, and Close waits for it only within ctx.
type mutant struct {
	inner mcp.Transport
	out   chan json.RawMessage
	stop  chan struct{} // closed when Close starts
	fwd   chan struct{} // closed when the forwarder has closed out

	transform func(json.RawMessage) []json.RawMessage
	// send, when set, replaces the forwarding of an outgoing message.
	send func(ctx context.Context, m *mutant, msg json.RawMessage) error
	// lateClose leaves Messages open for a while after Close returns.
	lateClose bool
	// err, when set, is what Err reports after Close.
	err error
	// closeOnceOnly makes every Close after the first fail.
	closeOnceOnly bool
	// ignoreCtx makes a Close whose ctx has already ended block for
	// slowCloseFor instead of returning ctx.Err(), as a Close that waits on
	// a slow release without watching ctx would. (A live ctx is not
	// delayed, so the other cases stay fast.)
	ignoreCtx bool

	stopOnce sync.Once
	closes   atomic.Int32
	closed   atomic.Bool
}

// notifyLag is how long a nil transform entry delays what follows it.
const notifyLag = 200 * time.Millisecond

// slowCloseFor is how long an ignoreCtx mutant's Close blocks.
const slowCloseFor = 5 * time.Second

// newMutant starts a mutant over a fresh in-process session; configure
// adjusts it before its forwarder starts.
func newMutant(configure func(m *mutant)) *mutant {
	m := &mutant{
		inner: mcp.InProcess(mcptest.NewServer()),
		out:   make(chan json.RawMessage),
		stop:  make(chan struct{}),
		fwd:   make(chan struct{}),
	}
	configure(m)
	go m.forward()
	return m
}

// forward delivers the inner transport's messages, transformed, until the
// inner transport ends; once Close starts it drops them instead.
func (m *mutant) forward() {
	defer close(m.fwd)
	for raw := range m.inner.Messages() {
		msgs := []json.RawMessage{raw}
		if m.transform != nil {
			msgs = m.transform(raw)
		}
		for _, msg := range msgs {
			if msg == nil {
				select {
				case <-time.After(notifyLag):
				case <-m.stop:
				}
				continue
			}
			select {
			case m.out <- msg:
			case <-m.stop:
			}
		}
	}
	if m.lateClose {
		time.Sleep(500 * time.Millisecond)
	}
	close(m.out)
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
	m.stopOnce.Do(func() { close(m.stop) })
	if m.ignoreCtx && ctx.Err() != nil {
		time.Sleep(slowCloseFor)
	}
	err := m.inner.Close(ctx)
	if err != nil || m.lateClose {
		return err
	}
	select {
	case <-m.fwd:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// heldNotifications holds notifications back until the next response.
// With lag set, they follow the response after a pause of notifyLag;
// otherwise they are dropped.
func heldNotifications(lag bool) func(json.RawMessage) []json.RawMessage {
	var held []json.RawMessage
	return func(raw json.RawMessage) []json.RawMessage {
		if bytes.Contains(raw, []byte(`"notifications/message"`)) {
			held = append(held, raw)
			return nil
		}
		out := []json.RawMessage{raw}
		if lag && len(held) > 0 {
			out = append(append(out, nil), held...)
		}
		held = nil
		return out
	}
}

// clobberConcurrent overwrites the echo text of a message sent while
// another Send is in flight, like a transport sharing one write buffer.
// It relies on Sends overlapping: the 2ms sleep keeps each Send in flight
// long enough that the kit's 8 concurrent callers always collide, while
// sequential callers (one Send at a time) never do.
func clobberConcurrent() func(ctx context.Context, m *mutant, msg json.RawMessage) error {
	var inFlight atomic.Int32
	return func(ctx context.Context, m *mutant, msg json.RawMessage) error {
		defer inFlight.Add(-1)
		if inFlight.Add(1) > 1 {
			msg = json.RawMessage(strings.Replace(string(msg), `"text":"`, `"text":"clobbered `, 1))
		}
		time.Sleep(2 * time.Millisecond)
		return m.inner.Send(ctx, msg)
	}
}

// afterClose makes Send return err once Close has been called.
func afterClose(err error) func(m *mutant) {
	return func(m *mutant) {
		m.send = func(ctx context.Context, m *mutant, msg json.RawMessage) error {
			if m.closed.Load() {
				return err
			}
			return m.inner.Send(ctx, msg)
		}
	}
}

// mutants maps "Case/how" to a configuration that breaks exactly the
// behaviour the kit case Case checks.
var mutants = map[string]func(m *mutant){
	"Handshake/wrongName":                 func(m *mutant) { m.transform = rewrite(`"name":"mcptest"`, `"name":"other"`) },
	"Sequential/staleResults":             func(m *mutant) { m.transform = staleResults() },
	"Concurrent/clobberedSend":            func(m *mutant) { m.send = clobberConcurrent() },
	"NotificationsBeforeReply/reversed":   func(m *mutant) { m.transform = reversedNotifications() },
	"NotificationsBeforeReply/afterReply": func(m *mutant) { m.transform = heldNotifications(true) },
	"NotificationsBeforeReply/dropped":    func(m *mutant) { m.transform = heldNotifications(false) },
	"CloseEndsMessages/stillOpen":         func(m *mutant) { m.lateClose = true },
	"CloseEndsMessages/errAfterClose":     func(m *mutant) { m.err = errors.New("mutant: closed") },
	"CloseIdempotent/secondCloseFails":    func(m *mutant) { m.closeOnceOnly = true },
	"CloseIdempotent/ignoresCtx":          func(m *mutant) { m.ignoreCtx = true },
	"SendAfterClose/silentDrop":           afterClose(nil),
	"SendAfterClose/wrongError":           afterClose(errors.New("mutant: send on closed transport")),
}

// alsoFails lists the other kit cases a mutant is expected to fail, beyond
// the one its name targets.
var alsoFails = map[string][]string{
	// Stale results break every echo after the first, concurrent ones too.
	"Sequential/staleResults": {"Concurrent"},
	// The connected variants close the transport twice: directly, then
	// through the client left open on it.
	"CloseIdempotent/secondCloseFails": {"CloseEndsMessages", "SendAfterClose"},
}

// failedCase matches the failure line of a top-level kit case.
var failedCase = regexp.MustCompile(`(?m)^\s*--- FAIL: TestBrokenTransportsAreFlagged/([A-Za-z]+) \(`)

// TestBrokenTransportsAreFlagged runs the kit in a child process against
// each mutant and checks that exactly the cases the mutant breaks fail.
func TestBrokenTransportsAreFlagged(t *testing.T) {
	if name := os.Getenv(brokenEnv); name != "" {
		mcptest.Run(t, func(t *testing.T) mcp.Transport {
			m := newMutant(mutants[name])
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
		target, _, _ := strings.Cut(name, "/")
		want := append([]string{target}, alsoFails[name]...)
		slices.Sort(want)
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
			var got []string
			for _, m := range failedCase.FindAllStringSubmatch(string(out), -1) {
				got = append(got, m[1])
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("failed cases = %v, want %v:\n%s", got, want, out)
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
