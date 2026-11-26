package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// rawSession is an initialized in-process session driven with raw messages.
func rawSession(t *testing.T, s *Server) Transport {
	t.Helper()
	sess := InProcess(s)
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	send(t, sess, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	<-sess.Messages()
	return sess
}

func send(t *testing.T, tr Transport, msg string) {
	t.Helper()
	if err := tr.Send(context.Background(), json.RawMessage(msg)); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func callBlock(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"block"}}`, id)
}

func cancelRequest(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%d}}`, id)
}

// requireCancelled waits for the block tool's handler to see its ctx end.
func requireCancelled(t *testing.T, blocked <-chan error, what string) {
	t.Helper()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatalf("%s: handler was not cancelled", what)
	}
}

func TestCancelRightAfterRequest(t *testing.T) {
	blocked := make(chan error, 1)
	sess := rawSession(t, newTestServer(blocked))
	for id := range 50 {
		send(t, sess, callBlock(id))
		send(t, sess, cancelRequest(id))
		requireCancelled(t, blocked, fmt.Sprintf("request %d", id))
	}
}

func TestCancelAfterRequestIDReuse(t *testing.T) {
	blocked := make(chan error, 1)
	sess := rawSession(t, newTestServer(blocked))
	for i := range 50 {
		send(t, sess, callBlock(1))
		send(t, sess, cancelRequest(1))
		requireCancelled(t, blocked, fmt.Sprintf("round %d, first request", i))
		// The first request's cleanup may still be running; it must not
		// forget the second request that reuses its id.
		send(t, sess, callBlock(1))
		time.Sleep(time.Millisecond)
		send(t, sess, cancelRequest(1))
		requireCancelled(t, blocked, fmt.Sprintf("round %d, reused id", i))
	}
}

func TestNotifyAfterSessionClosed(t *testing.T) {
	s := NewServer(Implementation{Name: "keeper", Version: "1"}, "")
	kept := make(chan context.Context, 1)
	s.AddTool(ToolInfo{Name: "keep"}, func(ctx context.Context, _ json.RawMessage) (*CallToolResult, error) {
		kept <- ctx
		return TextResult("ok"), nil
	})
	for range 20 {
		c, err := NewClient(context.Background(), InProcess(s), ClientConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CallTool(context.Background(), "keep", nil); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := Notify(<-kept, "notifications/progress", nil); err == nil {
			t.Fatal("Notify on a closed session succeeded")
		}
	}
}
