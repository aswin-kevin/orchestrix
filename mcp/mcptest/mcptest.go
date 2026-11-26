// Package mcptest is a conformance kit for mcp.Transport implementations.
//
// Run checks a transport against a NewServer() server: the handshake,
// sequential and concurrent requests, notifications delivered in order
// before the response of the request that sent them, and the Close
// contract (Messages closed and Err nil once Close returns nil, Close
// idempotent and safe for concurrent use, Send after Close failing with
// mcp.ErrClosed).
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
// Each subtest calls connect for its own transport, runs within 10 seconds
// and closes what it opened; connect may register cleanups of its own on
// the t it is given (the subtest's).
func Run(t *testing.T, connect func(t *testing.T) mcp.Transport) {
	t.Helper()
	cases := []struct {
		name string
		run  func(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport)
	}{
		{"Handshake", testHandshake},
		{"Sequential", testSequential},
		{"Concurrent", testConcurrent},
		{"NotificationsBeforeReply", testNotificationsBeforeReply},
		{"CloseEndsMessages", testCloseEndsMessages},
		{"CloseIdempotent", testCloseIdempotent},
		{"SendAfterClose", testSendAfterClose},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			ctx, cancel := context.WithTimeout(t.Context(), caseTimeout)
			defer cancel()
			c.run(t, ctx, connect)
		})
	}
}

// newClient connects a client over a fresh transport; it is closed when
// the test ends.
func newClient(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport, cfg mcp.ClientConfig) *mcp.Client {
	t.Helper()
	c, err := mcp.NewClient(ctx, connect(t), cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), caseTimeout)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Errorf("Client.Close: %v", err)
		}
	})
	return c
}

// echo calls the echo tool and returns its text.
func echo(ctx context.Context, c *mcp.Client, text string) (string, error) {
	args, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", err
	}
	res, err := c.CallTool(ctx, "echo", args)
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

// closeTransport closes tr within ctx and fails the test on an error.
func closeTransport(t *testing.T, ctx context.Context, tr mcp.Transport) {
	t.Helper()
	if err := tr.Close(ctx); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
}

func testHandshake(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport) {
	t.Helper()
	c := newClient(t, ctx, connect, mcp.ClientConfig{})
	if got := c.ServerInfo().Name; got != "mcptest" {
		t.Fatalf("ServerInfo().Name = %q, want %q", got, "mcptest")
	}
}

func testSequential(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport) {
	t.Helper()
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

func testConcurrent(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport) {
	t.Helper()
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

func testNotificationsBeforeReply(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport) {
	t.Helper()
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
	res, err := c.CallTool(ctx, "notify", json.RawMessage(`{}`))
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

func testCloseEndsMessages(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport) {
	t.Helper()
	tr := connect(t)
	closeTransport(t, ctx, tr)
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

func testCloseIdempotent(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport) {
	t.Helper()
	tr := connect(t)
	closeTransport(t, ctx, tr)
	if err := tr.Close(ctx); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}

	tr = connect(t)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = tr.Close(ctx) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close %d = %v, want nil", i, err)
		}
	}
	if err := tr.Close(ctx); err != nil {
		t.Fatalf("Close after concurrent Closes = %v, want nil", err)
	}
}

func testSendAfterClose(t *testing.T, ctx context.Context, connect func(t *testing.T) mcp.Transport) {
	t.Helper()
	tr := connect(t)
	closeTransport(t, ctx, tr)
	err := tr.Send(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if !errors.Is(err, mcp.ErrClosed) {
		t.Fatalf("Send after Close = %v, want mcp.ErrClosed", err)
	}
}
