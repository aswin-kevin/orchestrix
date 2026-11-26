// Package mcptest is a conformance kit for mcp.Transport implementations.
//
// Run checks a transport against a NewServer() server: the handshake,
// sequential and concurrent requests, notifications delivered in order
// before the response of the request that sent them, and the Close
// contract (Messages closed and Err nil once Close returns nil, Close
// idempotent, safe for concurrent use and bounded by its ctx, Send after
// Close failing with mcp.ErrClosed), both on a fresh transport and on one
// carrying a live session. Every blocking call is bounded: a transport
// that ignores ctx fails the case instead of hanging the test binary.
package mcptest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/mcp"
)

// caseTimeout bounds each Run subtest.
const caseTimeout = 10 * time.Second

// promptClose bounds a Close whose ctx has already ended: it must return
// at once, leaving any release to go on in the background.
const promptClose = time.Second

// notifyCount is how many notifications the "notify" tool sends.
const notifyCount = 20

// NewServer returns the fixture server every Run subtest talks to:
// Implementation{Name: "mcptest"}, tools "echo" (returns its "text"
// argument) and "notify" (sends "notifications/message" with {"n": i} for
// i in 0..19, then returns "done").
func NewServer() *mcp.Server {
	s := mcp.NewServer(mcp.Implementation{Name: "mcptest"}, "")
	s.AddTool(mcp.ToolInfo{
		Name:        "echo",
		Description: "Returns its text argument.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
		},
	}, func(_ context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		return mcp.TextResult(in.Text), nil
	})
	s.AddTool(mcp.ToolInfo{
		Name:        "notify",
		Description: `Sends 20 "notifications/message" notifications, then returns "done".`,
	}, func(ctx context.Context, _ json.RawMessage) (*mcp.CallToolResult, error) {
		for i := range notifyCount {
			if err := mcp.Notify(ctx, "notifications/message", map[string]int{"n": i}); err != nil {
				return nil, err
			}
		}
		return mcp.TextResult("done"), nil
	})
	return s
}

// Run checks a Transport implementation. connect must return a fresh,
// unused transport connected to a new session of a NewServer() server (in
// process, over HTTP, or a child process serving it over stdio).
//
// Each subtest calls connect for its own transports and runs within 10
// seconds; the Close cases run twice, on a fresh transport ("fresh") and
// on one after a handshake and a call ("connected"), each variant with its
// own 10 seconds. connect may register cleanups of its own on the t it is
// given.
func Run(t *testing.T, connect func(t *testing.T) mcp.Transport) {
	t.Helper()
	cases := []struct {
		name string
		run  func(t *testing.T, connect func(t *testing.T) mcp.Transport)
	}{
		{"Handshake", testHandshake},
		{"Sequential", testSequential},
		{"Concurrent", testConcurrent},
		{"NotificationsBeforeReply", testNotificationsBeforeReply},
		{"CloseEndsMessages", closeCase(testCloseEndsMessages)},
		{"CloseIdempotent", closeCase(testCloseIdempotent)},
		{"SendAfterClose", closeCase(testSendAfterClose)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, connect) })
	}
}

// caseCtx returns the ctx bounding one (sub)test.
func caseCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), caseTimeout)
	t.Cleanup(cancel)
	return ctx
}

// call runs fn in a goroutine and returns its error, or an error naming
// what when ctx ends first (fn is then left running).
func call(ctx context.Context, what string, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("mcptest: %s did not return within ctx: %w", what, ctx.Err())
	}
}

// within is call that fails the test when fn does not return within ctx;
// otherwise it returns fn's error. It must run on the test's goroutine.
func within(t *testing.T, ctx context.Context, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatalf("%s did not return within ctx (%v)", what, ctx.Err())
		return nil
	}
}

// closeCase runs a Close case on a fresh transport and on a connected one.
func closeCase(run func(t *testing.T, ctx context.Context, open func(t *testing.T) mcp.Transport)) func(*testing.T, func(*testing.T) mcp.Transport) {
	return func(t *testing.T, connect func(t *testing.T) mcp.Transport) {
		t.Run("fresh", func(t *testing.T) { run(t, caseCtx(t), connect) })
		t.Run("connected", func(t *testing.T) {
			ctx := caseCtx(t)
			run(t, ctx, func(t *testing.T) mcp.Transport { return connected(t, ctx, connect) })
		})
	}
}

// connected returns a transport carrying a live session: a client has
// completed the handshake and one echo call over it, and is left open (it
// is closed when the test ends, after the case closed the transport).
func connected(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport) mcp.Transport {
	t.Helper()
	tr := connect(t)
	c := clientOver(t, ctx, tr, mcp.ClientConfig{})
	if got, err := echo(ctx, c, "hello"); err != nil || got != "hello" {
		t.Fatalf("echo before Close = %q, %v; want \"hello\"", got, err)
	}
	return tr
}

// newClient connects a client over a fresh transport; it is closed when
// the test ends.
func newClient(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport, cfg mcp.ClientConfig) *mcp.Client {
	t.Helper()
	return clientOver(t, ctx, connect(t), cfg)
}

// clientOver connects a client over tr; it is closed when the test ends.
func clientOver(t *testing.T, ctx context.Context, tr mcp.Transport, cfg mcp.ClientConfig) *mcp.Client {
	t.Helper()
	var c *mcp.Client
	err := within(t, ctx, "NewClient", func() error {
		var err error
		c, err = mcp.NewClient(ctx, tr, cfg)
		return err
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), caseTimeout)
		defer cancel()
		if err := call(ctx, "Client.Close", func() error { return c.Close(ctx) }); err != nil {
			t.Errorf("Client.Close: %v", err)
		}
	})
	return c
}

// echo calls the echo tool and returns its text. It may run on any
// goroutine: a call that does not return within ctx is an error.
func echo(ctx context.Context, c *mcp.Client, text string) (string, error) {
	args, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", err
	}
	var res *mcp.CallToolResult
	err = call(ctx, "CallTool(echo)", func() error {
		var err error
		res, err = c.CallTool(ctx, "echo", args)
		return err
	})
	if err != nil {
		return "", err
	}
	return resultText(res)
}

// resultText returns the text of a result that is one text block.
func resultText(res *mcp.CallToolResult) (string, error) {
	if res.IsError || len(res.Content) != 1 || res.Content[0].Type != "text" {
		return "", fmt.Errorf("mcptest: result %+v is not one text block", res)
	}
	return res.Content[0].Text, nil
}

// closeTransport closes tr within ctx and fails the test unless Close
// returns nil.
func closeTransport(t *testing.T, ctx context.Context, tr mcp.Transport, what string) {
	t.Helper()
	if err := within(t, ctx, what, func() error { return tr.Close(ctx) }); err != nil {
		t.Fatalf("%s = %v, want nil", what, err)
	}
}

func testHandshake(t *testing.T, connect func(t *testing.T) mcp.Transport) {
	c := newClient(t, caseCtx(t), connect, mcp.ClientConfig{})
	if got := c.ServerInfo().Name; got != "mcptest" {
		t.Fatalf("ServerInfo().Name = %q, want %q", got, "mcptest")
	}
}

func testSequential(t *testing.T, connect func(t *testing.T) mcp.Transport) {
	ctx := caseCtx(t)
	c := newClient(t, ctx, connect, mcp.ClientConfig{})
	for i := range 50 {
		want := fmt.Sprintf("seq-%d", i)
		got, err := echo(ctx, c, want)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("call %d returned %q, want %q", i, got, want)
		}
	}
}

func testConcurrent(t *testing.T, connect func(t *testing.T) mcp.Transport) {
	ctx := caseCtx(t)
	c := newClient(t, ctx, connect, mcp.ClientConfig{})
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for g := range 8 {
		wg.Go(func() {
			for i := range 10 {
				want := fmt.Sprintf("conc-%d-%d", g, i)
				got, err := echo(ctx, c, want)
				if err == nil && got != want {
					err = fmt.Errorf("returned %q", got)
				}
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("call %s: %w", want, err))
					mu.Unlock()
					return
				}
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
}

func testNotificationsBeforeReply(t *testing.T, connect func(t *testing.T) mcp.Transport) {
	ctx := caseCtx(t)
	var (
		mu   sync.Mutex
		seen []int
	)
	c := newClient(t, ctx, connect, mcp.ClientConfig{
		OnNotification: func(method string, params json.RawMessage) {
			if method != "notifications/message" {
				return
			}
			var p struct {
				N int `json:"n"`
			}
			n := -1
			if json.Unmarshal(params, &p) == nil {
				n = p.N
			}
			mu.Lock()
			seen = append(seen, n)
			mu.Unlock()
		},
	})
	var res *mcp.CallToolResult
	err := within(t, ctx, "CallTool(notify)", func() error {
		var err error
		res, err = c.CallTool(ctx, "notify", json.RawMessage(`{}`))
		return err
	})
	if err != nil {
		t.Fatalf("CallTool(notify): %v", err)
	}
	mu.Lock()
	got := slices.Clone(seen)
	mu.Unlock()
	if text, err := resultText(res); err != nil || text != "done" {
		t.Fatalf("notify returned %q, %v; want \"done\"", text, err)
	}
	want := make([]int, notifyCount)
	for i := range want {
		want[i] = i
	}
	if !slices.Equal(got, want) {
		t.Fatalf("notifications seen before the reply = %v, want %v", got, want)
	}
}

func testCloseEndsMessages(t *testing.T, ctx context.Context, open func(t *testing.T) mcp.Transport) {
	tr := open(t)
	closeTransport(t, ctx, tr, "Close")
	msgs := tr.Messages()
	for {
		select {
		case _, ok := <-msgs:
			if ok {
				continue // a message received before Close
			}
			if err := tr.Err(); err != nil {
				t.Fatalf("Err after Close = %v, want nil", err)
			}
			return
		default:
			t.Fatal("Messages still open after Close returned nil")
		}
	}
}

func testCloseIdempotent(t *testing.T, ctx context.Context, open func(t *testing.T) mcp.Transport) {
	// A Close whose ctx has ended returns at once, with nil (the release
	// was quick) or ctx.Err(); a later Close with a live ctx returns nil.
	tr := open(t)
	ended, cancel := context.WithCancel(ctx)
	cancel()
	prompt, cancelPrompt := context.WithTimeout(ctx, promptClose)
	defer cancelPrompt()
	err := within(t, prompt, "Close with an ended ctx", func() error { return tr.Close(ended) })
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with an ended ctx = %v, want nil or context.Canceled", err)
	}
	closeTransport(t, ctx, tr, "Close after a Close with an ended ctx")
	closeTransport(t, ctx, tr, "second Close")

	tr = open(t)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = call(ctx, "concurrent Close", func() error { return tr.Close(ctx) }) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close %d = %v, want nil", i, err)
		}
	}
	closeTransport(t, ctx, tr, "Close after concurrent Closes")
}

func testSendAfterClose(t *testing.T, ctx context.Context, open func(t *testing.T) mcp.Transport) {
	tr := open(t)
	closeTransport(t, ctx, tr, "Close")
	err := within(t, ctx, "Send after Close", func() error {
		return tr.Send(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":99,"method":"ping"}`))
	})
	if !errors.Is(err, mcp.ErrClosed) {
		t.Fatalf("Send after Close = %v, want mcp.ErrClosed", err)
	}
}
