package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/internal/version"
)

// connect returns a client for s over an in-process session, closed when the
// test ends.
func connect(t *testing.T, s *Server, opts ClientConfig) *Client {
	t.Helper()
	c, err := NewClient(context.Background(), InProcess(s), opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestInProcessRoundTrip(t *testing.T) {
	c := connect(t, newTestServer(nil), ClientConfig{})
	ctx := t.Context()

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
	c := connect(t, s, ClientConfig{})

	tools, err := c.ListTools(t.Context())
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
	c := connect(t, newTestServer(blocked), ClientConfig{RequestTimeout: 200 * time.Millisecond})

	start := time.Now()
	_, err := c.CallTool(t.Context(), "block", nil)
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

	res, err := c.CallTool(t.Context(), "echo", json.RawMessage(`{"text":"still here"}`))
	if err != nil {
		t.Fatalf("call after timeout: %v", err)
	}
	if got := resultText(t, res); got != "still here" {
		t.Errorf("echo = %q", got)
	}
}

func TestCallCtxCancelIsNotTimeout(t *testing.T) {
	c := connect(t, newTestServer(nil), ClientConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := c.CallTool(ctx, "block", nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want context.Canceled and not ErrTimeout", err)
	}
}

func TestToolListChangedNotifies(t *testing.T) {
	s := newTestServer(nil)
	notes := make(chan string, 4)
	connect(t, s, ClientConfig{OnNotification: func(method string, _ json.RawMessage) { notes <- method }})

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
	c := connect(t, s, ClientConfig{OnNotification: func(method string, params json.RawMessage) {
		notes <- method + " " + string(params)
	}})

	res, err := c.CallTool(t.Context(), "progress", nil)
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
	if err := Notify(t.Context(), "x", nil); err == nil {
		t.Error("Notify outside a handler succeeded")
	}
}

func TestPingFromServerAnswered(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, ProtocolVersion) })
	c, err := NewClient(context.Background(), f, ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close(context.Background())

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
	_, err := NewClient(t.Context(), f, ClientConfig{})
	if err == nil || !strings.Contains(err.Error(), "1999-01-01") {
		t.Fatalf("err = %v, want an error naming the unsupported version", err)
	}
	if !f.isClosed() {
		t.Error("transport left open after a failed initialize")
	}
}

func TestInitializeHandshake(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, "2025-03-26") })
	c, err := NewClient(context.Background(), f, ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close(context.Background())

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
	defer sess.Close(context.Background())
	for _, tt := range []struct{ asked, want string }{
		{"2024-11-05", "2024-11-05"},
		{"2099-01-01", ProtocolVersion},
	} {
		msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`, tt.asked)
		if err := sess.Send(t.Context(), json.RawMessage(msg)); err != nil {
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
		if reply.Result.ProtocolVersion != tt.want {
			t.Errorf("asked %s: got %s, want %s", tt.asked, reply.Result.ProtocolVersion, tt.want)
		}
	}
}

func TestClosedClientFailsCalls(t *testing.T) {
	c := connect(t, newTestServer(nil), ClientConfig{})
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done not closed after Close")
	}
	if _, err := c.ListTools(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestReplyBeforeEndIsDelivered(t *testing.T) {
	// The server answers and the transport ends at once: the answer must win
	// over the connection error.
	for i := range 50 {
		f := newFakeTransport(func(msg map[string]any) []string {
			if msg["method"] != "tools/list" {
				return initializeReply(msg, ProtocolVersion)
			}
			id, _ := json.Marshal(msg["id"])
			return []string{fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"t","inputSchema":{}}]}}`, id)}
		})
		f.endAfter = isMethod("tools/list")
		c, err := NewClient(t.Context(), f, ClientConfig{})
		if err != nil {
			t.Fatal(err)
		}
		tools, err := c.ListTools(t.Context())
		_ = c.Close(t.Context())
		if err != nil || len(tools) != 1 {
			t.Fatalf("iteration %d: ListTools = %v, %v; want the reply that arrived before the end", i, tools, err)
		}
	}
}

func TestCancelSentWhenSendTimesOut(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, ProtocolVersion) })
	f.hold = isMethod("tools/call")
	c, err := NewClient(context.Background(), f, ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, "slow", nil); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	call := f.waitSent(t, isMethod("tools/call"))
	note := f.waitSent(t, isMethod("notifications/cancelled"))
	params, _ := note["params"].(map[string]any)
	if params["requestId"] != call["id"] {
		t.Errorf("cancelled requestId = %v, want the call's id %v", params["requestId"], call["id"])
	}
}

// isResponse matches messages that answer a request.
func isResponse(msg map[string]any) bool {
	_, hasMethod := msg["method"]
	return !hasMethod
}

func TestServerRequestFloodBounded(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string {
		if msg["method"] == "tools/call" {
			id, _ := json.Marshal(msg["id"])
			return []string{fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, id)}
		}
		return initializeReply(msg, ProtocolVersion)
	})
	// Every reply to the server gets stuck in the transport, so replies pile
	// up unless the client bounds them.
	f.hold = isResponse
	c, err := NewClient(context.Background(), f, ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())

	baseline := runtime.NumGoroutine()
	limit := baseline + 10
	const pings = 100_000
	for i := range pings {
		f.push(fmt.Sprintf(`{"jsonrpc":"2.0","id":"p%d","method":"ping"}`, i))
		if i%5000 == 0 {
			if n := runtime.NumGoroutine(); n >= limit {
				t.Fatalf("after %d pings: %d goroutines, baseline %d", i, n, baseline)
			}
		}
	}
	// Once the reader has taken the last ping, the count must have settled.
	for range 5 {
		time.Sleep(20 * time.Millisecond)
		if n := runtime.NumGoroutine(); n >= limit {
			t.Fatalf("after the flood: %d goroutines, baseline %d", n, baseline)
		}
	}

	res, err := c.CallTool(t.Context(), "echo", nil)
	if err != nil {
		t.Fatalf("CallTool after the flood: %v", err)
	}
	if got := resultText(t, res); got != "ok" {
		t.Errorf("CallTool after the flood = %q, want ok", got)
	}
}

func TestCloseWithFullReplyQueue(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, ProtocolVersion) })
	f.hold = isResponse
	c, err := NewClient(t.Context(), f, ClientConfig{RequestTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		f.push(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`, i))
	}
	f.waitSent(t, isResponse) // the replier is stuck in Send
	closed := make(chan error, 1)
	go func() { closed <- c.Close(t.Context()) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return with a full reply queue and a stuck transport")
	}
}

func TestVersionUsed(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, ProtocolVersion) })
	c, err := NewClient(context.Background(), f, ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	init := f.waitSent(t, isMethod("initialize"))
	params, _ := init["params"].(map[string]any)
	info, _ := params["clientInfo"].(map[string]any)
	if info["version"] != version.Version {
		t.Errorf("clientInfo.version = %v, want %q", info["version"], version.Version)
	}
}

func TestSupportedVersionsIsCopy(t *testing.T) {
	v := SupportedVersions()
	v[0] = "mutated"
	if got := SupportedVersions()[0]; got != ProtocolVersion {
		t.Errorf("SupportedVersions()[0] = %q after mutation, want %q", got, ProtocolVersion)
	}
}

// versionRecorder wraps InProcess and records the negotiated version.
type versionRecorder struct {
	Transport
	version atomic.Value
}

func (v *versionRecorder) SetProtocolVersion(version string) { v.version.Store(version) }

func TestProtocolVersionSetterCalled(t *testing.T) {
	var _ ProtocolVersionSetter = (*versionRecorder)(nil)
	v := &versionRecorder{Transport: InProcess(newTestServer(nil))}
	c, err := NewClient(context.Background(), v, ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	if got := v.version.Load(); got != ProtocolVersion {
		t.Errorf("SetProtocolVersion got %v, want %q", got, ProtocolVersion)
	}
}

func TestClientCloseIdempotent(t *testing.T) {
	c := connect(t, newTestServer(nil), ClientConfig{})
	for range 2 {
		if err := c.Close(t.Context()); err != nil {
			t.Fatalf("sequential Close: %v", err)
		}
	}
	c = connect(t, newTestServer(nil), ClientConfig{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { errs <- c.Close(t.Context()) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Close: %v", err)
		}
	}
	select {
	case <-c.Done():
	default:
		t.Error("Done not closed after Close returned nil")
	}
}

func TestNewClientRejectsNegativeRequestTimeout(t *testing.T) {
	f := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, ProtocolVersion) })
	_, err := NewClient(t.Context(), f, ClientConfig{RequestTimeout: -time.Second})
	if err == nil || err.Error() != "mcp: RequestTimeout is negative" {
		t.Fatalf("err = %v, want mcp: RequestTimeout is negative", err)
	}
	if !f.isClosed() {
		t.Error("transport left open after rejection")
	}
}

// deadlineRecorder is a Transport whose Close records the state of the
// ctx it got, when it got it.
type deadlineRecorder struct {
	*fakeTransport
	ctxs chan closeCtx
}

// closeCtx is the state of a ctx passed to Close.
type closeCtx struct {
	err         error
	hasDeadline bool
}

func (r *deadlineRecorder) Close(ctx context.Context) error {
	_, ok := ctx.Deadline()
	select {
	case r.ctxs <- closeCtx{ctx.Err(), ok}:
	default:
	}
	return r.fakeTransport.Close(ctx)
}

// The cleanup Close on a failure path drops the caller's cancellation but
// is bounded, so a stuck transport cannot hang the caller. After a failed
// initialize the bound applies to Client.Close, which returns when its ctx
// ends; the client hands its transport a ctx without cancellation.
func TestFailureCleanupCloseIsBounded(t *testing.T) {
	newRecorder := func() *deadlineRecorder {
		return &deadlineRecorder{
			fakeTransport: newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, ProtocolVersion) }),
			ctxs:          make(chan closeCtx, 1),
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tt := range []struct {
		name    string
		bounded bool // the transport's own Close gets a deadline
		run     func(*deadlineRecorder) error
	}{
		{"negative RequestTimeout", true, func(r *deadlineRecorder) error {
			_, err := NewClient(t.Context(), r, ClientConfig{RequestTimeout: -time.Second})
			return err
		}},
		{"failed initialize", false, func(r *deadlineRecorder) error {
			_, err := NewClient(cancelled, r, ClientConfig{})
			return err
		}},
		{"invalid servers", true, func(r *deadlineRecorder) error {
			_, err := Connect(cancelled, []ServerConfig{{Name: "a", Transport: r}, {Name: "a", Transport: r}}, ConnectConfig{})
			return err
		}},
	} {
		r := newRecorder()
		if err := tt.run(r); err == nil {
			t.Errorf("%s: no error", tt.name)
			continue
		}
		select {
		case got := <-r.ctxs:
			if got.err != nil || got.hasDeadline != tt.bounded {
				t.Errorf("%s: cleanup Close ctx err %v, deadline %v; want no error, deadline %v",
					tt.name, got.err, got.hasDeadline, tt.bounded)
			}
		default:
			t.Errorf("%s: transport not closed", tt.name)
		}
	}
}
