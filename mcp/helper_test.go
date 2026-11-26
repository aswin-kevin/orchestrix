package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// bigResultBytes is the size of the "big" tool's text, larger than the
// message cap the oversize test sets.
const bigResultBytes = 4 << 10

// newTestServer returns a server with the tools every test mode shares:
// echo (returns its "text" argument), block (waits for its ctx to end and
// reports that on blocked), big (returns bigResultBytes of text) and fail
// (handler error), plus one text resource.
func newTestServer(blocked chan<- error) *Server {
	s := NewServer(Implementation{Name: "test-server", Version: "1.2.3"}, "use the echo tool")
	s.AddTool(ToolInfo{
		Name:        "echo",
		Description: "Echoes text.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}, func(_ context.Context, args json.RawMessage) (*CallToolResult, error) {
		var in struct{ Text string }
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		return TextResult(in.Text), nil
	})
	s.AddTool(ToolInfo{Name: "block"}, func(ctx context.Context, _ json.RawMessage) (*CallToolResult, error) {
		<-ctx.Done()
		if blocked != nil {
			blocked <- ctx.Err()
		}
		return nil, ctx.Err()
	})
	s.AddTool(ToolInfo{Name: "big"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
		return TextResult(strings.Repeat("x", bigResultBytes)), nil
	})
	s.AddTool(ToolInfo{Name: "fail"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
		return nil, errors.New("it broke")
	})
	s.AddResource(Resource{URI: "file:///readme", Name: "readme", MimeType: "text/plain"},
		func(context.Context) ([]ResourceContent, error) {
			return []ResourceContent{{URI: "file:///readme", MimeType: "text/plain", Text: "hello"}}, nil
		})
	return s
}

// resultText returns the text of a single-text-block result.
func resultText(t *testing.T, res *CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want one text block", res.Content)
	}
	return res.Content[0].Text
}

// fakeTransport is a scripted Transport: every message the client sends is
// passed to reply, whose returned messages are delivered back.
type fakeTransport struct {
	reply func(msg map[string]any) []string
	// hold, when set and true for a message, makes Send record it and then
	// block until its ctx ends, like a transport stuck after queueing.
	hold func(msg map[string]any) bool
	// endAfter, when set and true for a message, ends the transport right
	// after its replies are delivered, then gives the client's reader time to
	// see both before Send returns.
	endAfter func(msg map[string]any) bool

	mu     sync.Mutex
	sent   []map[string]any
	msgs   chan json.RawMessage
	once   sync.Once
	closed chan struct{}
}

func newFakeTransport(reply func(msg map[string]any) []string) *fakeTransport {
	return &fakeTransport{reply: reply, msgs: make(chan json.RawMessage, 16), closed: make(chan struct{})}
}

func (f *fakeTransport) Send(ctx context.Context, raw json.RawMessage) error {
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}
	f.mu.Lock()
	select {
	case <-f.closed:
		f.mu.Unlock()
		return ErrClosed
	default:
	}
	f.sent = append(f.sent, msg)
	if f.hold != nil && f.hold(msg) {
		f.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	for _, out := range f.reply(msg) {
		f.msgs <- json.RawMessage(out)
	}
	end := f.endAfter != nil && f.endAfter(msg)
	f.mu.Unlock()
	if end {
		_ = f.Close(context.Background())
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// push delivers a message as if the server had sent it unprompted.
func (f *fakeTransport) push(raw string) { f.msgs <- json.RawMessage(raw) }

func (f *fakeTransport) Messages() <-chan json.RawMessage { return f.msgs }
func (f *fakeTransport) Err() error                       { return nil }

func (f *fakeTransport) Close(context.Context) error {
	f.once.Do(func() {
		f.mu.Lock()
		close(f.closed)
		close(f.msgs)
		f.mu.Unlock()
	})
	return nil
}

func (f *fakeTransport) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

// waitSent waits until a sent message satisfies match and returns it.
func (f *fakeTransport) waitSent(t *testing.T, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, msg := range f.sent {
			if match(msg) {
				f.mu.Unlock()
				return msg
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected message was never sent")
	return nil
}

// initializeReply answers an initialize request with protocol version v.
func initializeReply(msg map[string]any, v string) []string {
	if msg["method"] != "initialize" {
		return nil
	}
	id, _ := json.Marshal(msg["id"])
	return []string{fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"fake","version":"0"}}}`, id, v)}
}

// isMethod matches messages calling method.
func isMethod(method string) func(map[string]any) bool {
	return func(msg map[string]any) bool { return msg["method"] == method }
}
