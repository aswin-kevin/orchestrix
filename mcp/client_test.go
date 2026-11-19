package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// connect returns a client for s over an in-process session, closed when the
// test ends.
func connect(t *testing.T, s *Server, opts ClientOptions) *Client {
	t.Helper()
	c, err := NewClient(context.Background(), InProcess(s), opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestInProcessRoundTrip(t *testing.T) {
	c := connect(t, newTestServer(nil), ClientOptions{})
	ctx := context.Background()

	if got := c.ServerInfo(); got != (Implementation{Name: "test-server", Version: "1.2.3"}) {
		t.Errorf("ServerInfo = %+v", got)
	}
	if got := c.Instructions(); got != "use the echo tool" {
		t.Errorf("Instructions = %q", got)
	}
	if !c.HasTools() || !c.HasResources() {
		t.Errorf("HasTools = %v, HasResources = %v, want both", c.HasTools(), c.HasResources())
	}

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 4 || tools[0].Name != "echo" || tools[0].Description != "Echoes text." {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].InputSchema["type"] != "object" {
		t.Errorf("echo schema = %v", tools[0].InputSchema)
	}

	res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi there"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := resultText(t, res); got != "hi there" || res.IsError {
		t.Errorf("echo = %q (isError %v)", got, res.IsError)
	}

	res, err = c.CallTool(ctx, "fail", nil)
	if err != nil {
		t.Fatalf("CallTool fail: %v", err)
	}
	if got := resultText(t, res); !res.IsError || got != "it broke" {
		t.Errorf("fail = %q (isError %v), want the handler error as an error result", got, res.IsError)
	}

	var rpcErr *RPCError
	if _, err := c.CallTool(ctx, "nope", nil); !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
		t.Errorf("unknown tool error = %v, want RPCError -32602", err)
	}

	resources, err := c.ListResources(ctx)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources) != 1 || resources[0].URI != "file:///readme" || resources[0].MimeType != "text/plain" {
		t.Fatalf("resources = %+v", resources)
	}
	contents, err := c.ReadResource(ctx, "file:///readme")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(contents) != 1 || contents[0].Text != "hello" {
		t.Errorf("contents = %+v", contents)
	}
}

func TestListToolsPagination(t *testing.T) {
	s := NewServer(Implementation{Name: "paged", Version: "1"}, "")
	s.pageSize = 2
	for i := range 5 {
		s.AddTool(ToolInfo{Name: fmt.Sprintf("t%d", i)}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
			return TextResult("ok"), nil
		})
	}
	c := connect(t, s, ClientOptions{})

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "t0,t1,t2,t3,t4" {
		t.Fatalf("tools = %s, want t0..t4 in order", got)
	}
}

func TestCallTimeoutCancels(t *testing.T) {
	blocked := make(chan error, 1)
	c := connect(t, newTestServer(blocked), ClientOptions{RequestTimeout: 200 * time.Millisecond})

	start := time.Now()
	_, err := c.CallTool(context.Background(), "block", nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timed-out call returned after %v", elapsed)
	}
	select {
	case cause := <-blocked:
		if !errors.Is(cause, context.Canceled) {
			t.Errorf("handler ctx ended with %v, want cancellation", cause)
		}
	case <-time.After(time.Second):
		t.Fatal("handler ctx was not cancelled by notifications/cancelled")
	}

	res, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"still here"}`))
	if err != nil {
		t.Fatalf("call after timeout: %v", err)
	}
	if got := resultText(t, res); got != "still here" {
		t.Errorf("echo = %q", got)
	}
}

func TestCallCtxCancelIsNotTimeout(t *testing.T) {
	c := connect(t, newTestServer(nil), ClientOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := c.CallTool(ctx, "block", nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want context.Canceled and not ErrTimeout", err)
	}
}

func TestToolListChangedNotifies(t *testing.T) {
	s := newTestServer(nil)
	notes := make(chan string, 4)
	connect(t, s, ClientOptions{OnNotification: func(method string, _ json.RawMessage) { notes <- method }})

	s.AddTool(ToolInfo{Name: "late"}, func(context.Context, json.RawMessage) (*CallToolResult, error) { return nil, nil })
	select {
	case got := <-notes:
		if got != "notifications/tools/list_changed" {
			t.Fatalf("notification = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no list_changed notification after AddTool")
	}
	s.RemoveTool("late")
	select {
	case <-notes:
	case <-time.After(2 * time.Second):
		t.Fatal("no list_changed notification after RemoveTool")
	}
}

func TestHandlerNotificationDelivered(t *testing.T) {
	s := NewServer(Implementation{Name: "notifier", Version: "1"}, "")
	s.AddTool(ToolInfo{Name: "progress"}, func(ctx context.Context, _ json.RawMessage) (*CallToolResult, error) {
		if err := Notify(ctx, "notifications/progress", map[string]any{"progress": 1}); err != nil {
			return nil, err
		}
		return TextResult("done"), nil
	})
	notes := make(chan string, 1)
	c := connect(t, s, ClientOptions{OnNotification: func(method string, params json.RawMessage) {
		notes <- method + " " + string(params)
	}})

	res, err := c.CallTool(context.Background(), "progress", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(t, res); got != "done" {
		t.Errorf("result = %q", got)
	}
	select {
	case got := <-notes:
		if got != `notifications/progress {"progress":1}` {
			t.Errorf("notification = %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("handler notification not delivered")
	}
	if err := Notify(context.Background(), "x", nil); err == nil {
		t.Error("Notify outside a handler succeeded")
	}
}

func TestPingFromServerAnswered(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, ProtocolVersion) })
	c, err := NewClient(context.Background(), f, ClientOptions{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	f.push(`{"jsonrpc":"2.0","id":"p1","method":"ping"}`)
	f.push(`{"jsonrpc":"2.0","id":7,"method":"sampling/createMessage","params":{}}`)

	pong := f.waitSent(t, func(m map[string]any) bool { return m["id"] == "p1" })
	if result, ok := pong["result"].(map[string]any); !ok || len(result) != 0 || pong["jsonrpc"] != "2.0" {
		t.Errorf("ping reply = %v, want an empty result", pong)
	}
	refusal := f.waitSent(t, func(m map[string]any) bool { return m["id"] == float64(7) })
	rpcErr, _ := refusal["error"].(map[string]any)
	if rpcErr["code"] != float64(-32601) || rpcErr["message"] != "method not found" {
		t.Errorf("unknown request reply = %v, want -32601 method not found", refusal)
	}
}

func TestUnsupportedVersionRejected(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, "1999-01-01") })
	_, err := NewClient(context.Background(), f, ClientOptions{})
	if err == nil || !strings.Contains(err.Error(), "1999-01-01") {
		t.Fatalf("err = %v, want an error naming the unsupported version", err)
	}
	if !f.isClosed() {
		t.Error("transport left open after a failed initialize")
	}
}

func TestInitializeHandshake(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, "2025-03-26") })
	c, err := NewClient(context.Background(), f, ClientOptions{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	init := f.waitSent(t, func(m map[string]any) bool { return m["method"] == "initialize" })
	params, _ := init["params"].(map[string]any)
	info, _ := params["clientInfo"].(map[string]any)
	if params["protocolVersion"] != ProtocolVersion || info["name"] != "orchestrix" || info["version"] == "" {
		t.Errorf("initialize params = %v", params)
	}
	if caps, ok := params["capabilities"].(map[string]any); !ok || len(caps) != 0 {
		t.Errorf("capabilities = %v, want {}", params["capabilities"])
	}
	note := f.waitSent(t, func(m map[string]any) bool { return m["method"] == "notifications/initialized" })
	if _, hasID := note["id"]; hasID {
		t.Errorf("initialized notification carries an id: %v", note)
	}
	if c.HasTools() || c.HasResources() {
		t.Error("capabilities reported that the server did not declare")
	}
}

func TestServerEchoesSupportedVersion(t *testing.T) {
	sess := InProcess(newTestServer(nil))
	defer sess.Close()
	for _, tc := range []struct{ asked, want string }{
		{"2024-11-05", "2024-11-05"},
		{"2099-01-01", ProtocolVersion},
	} {
		msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`, tc.asked)
		if err := sess.Send(context.Background(), json.RawMessage(msg)); err != nil {
			t.Fatal(err)
		}
		var reply struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if err := json.Unmarshal(<-sess.Messages(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Result.ProtocolVersion != tc.want {
			t.Errorf("asked %s: got %s, want %s", tc.asked, reply.Result.ProtocolVersion, tc.want)
		}
	}
}

func TestClosedClientFailsCalls(t *testing.T) {
	c := connect(t, newTestServer(nil), ClientOptions{})
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done not closed after Close")
	}
	if _, err := c.ListTools(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
