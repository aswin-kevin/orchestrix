package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// imagesOK is the profile of a model that takes images.
var imagesOK = llm.Profile{Images: true}

func TestToolName(t *testing.T) {
	if got := ToolName("srv", "get.file/v2"); got != "mcp__srv__get_file_v2" {
		t.Fatalf("ToolName = %q", got)
	}
	base := strings.Repeat("x", 99)
	a, b := ToolName("srv", base+"a"), ToolName("srv", base+"b")
	if a == b {
		t.Fatalf("long names collide: %q", a)
	}
	for _, n := range []string{a, b} {
		if len(n) != MaxToolNameLen {
			t.Fatalf("len(%q) = %d, want %d", n, len(n), MaxToolNameLen)
		}
		if !strings.HasPrefix(n, "mcp__srv__") {
			t.Fatalf("%q lost the server prefix", n)
		}
	}
	if again := ToolName("srv", base+"a"); again != a {
		t.Fatalf("ToolName not deterministic: %q then %q", a, again)
	}
}

func TestToolAdapterSchemaAndDescription(t *testing.T) {
	s := NewServer(Implementation{Name: "s", Version: "1"}, "")
	s.AddTool(ToolInfo{Name: "titled", Title: "Nice Title", Description: "Does things.", InputSchema: map[string]any{"type": "string"}}, nil)
	s.AddTool(ToolInfo{Name: "wordy", Description: strings.Repeat("d", MaxDescriptionBytes+10)}, nil)
	huge := map[string]any{"type": "object", "description": strings.Repeat("s", MaxSchemaBytes)}
	s.AddTool(ToolInfo{Name: "huge", InputSchema: huge}, nil)
	m := connectMiddleware(t, inProcessServer("srv", s))
	tools := m.DynamicTools()

	titled := findTool(t, tools, "mcp__srv__titled")
	if titled.Description() != "Nice Title: Does things." {
		t.Fatalf("description = %q", titled.Description())
	}
	objectOnly := map[string]any{"type": "object"}
	if !reflect.DeepEqual(titled.Schema(), objectOnly) {
		t.Fatalf("non-object schema = %v, want %v", titled.Schema(), objectOnly)
	}
	if r, ok := tool.As[tool.RawInputer](titled); !ok || !r.AcceptsRawInput() {
		t.Fatal("MCP tools must accept raw input")
	}
	if isReadOnly(titled, nil) || isConcurrent(titled, nil) {
		t.Fatal("MCP tools must not be read-only or concurrent by default")
	}
	if n := len(findTool(t, tools, "mcp__srv__wordy").Description()); n != MaxDescriptionBytes {
		t.Fatalf("description length = %d, want %d", n, MaxDescriptionBytes)
	}
	if got := findTool(t, tools, "mcp__srv__huge").Schema(); !reflect.DeepEqual(got, objectOnly) {
		t.Fatal("oversized schema not replaced")
	}
}

func TestContentMapping(t *testing.T) {
	image := llm.Block{Type: llm.BlockImage, Source: &llm.Source{Type: "base64", MediaType: "image/png", Data: pngData}}
	for _, tt := range []struct {
		name string
		in   CallToolResult
		want tool.Result
	}{
		{"text", CallToolResult{Content: []Content{{Type: "text", Text: "hello"}}},
			tool.Text("hello")},
		{"image", CallToolResult{Content: []Content{{Type: "image", Data: pngData, MimeType: "image/png"}}},
			tool.Result{Content: []llm.Block{image}}},
		{"audio", CallToolResult{Content: []Content{{Type: "audio", Data: "YXU=", MimeType: "audio/wav"}}},
			tool.Text("[audio content omitted: audio/wav]")},
		{"resource text", CallToolResult{Content: []Content{{Type: "resource", Resource: &ResourceContent{URI: "file:///a", Text: "body"}}}},
			tool.Text("Resource file:///a:\nbody")},
		{"resource blob image", CallToolResult{Content: []Content{{Type: "resource", Resource: &ResourceContent{URI: "file:///i", MimeType: "image/png", Blob: pngData}}}},
			tool.Result{Content: []llm.Block{image}}},
		{"resource blob other", CallToolResult{Content: []Content{{Type: "resource", Resource: &ResourceContent{URI: "file:///z", MimeType: "application/zip", Blob: "eg=="}}}},
			tool.Text("[binary resource file:///z omitted: application/zip]")},
		{"resource link", CallToolResult{Content: []Content{{Type: "resource_link", URI: "file:///l", Name: "lnk"}}},
			tool.Text("Resource link: file:///l (lnk)")},
		{"structured only", CallToolResult{StructuredContent: json.RawMessage(`{ "a" : [1, 2] }`)},
			tool.Text(`{"a":[1,2]}`)},
		{"error", CallToolResult{Content: []Content{{Type: "text", Text: "bad"}}, IsError: true},
			tool.Errorf("bad")},
		{"mixed", CallToolResult{Content: []Content{{Type: "text", Text: "a"}, {Type: "text", Text: "b"}}, StructuredContent: json.RawMessage(`{}`)},
			tool.Result{Content: []llm.Block{llm.TextBlock("a"), llm.TextBlock("b")}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolResult(&tt.in, imagesOK); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("result = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Small payloads with the magic bytes http.DetectContentType recognises.
var (
	pngBytes  = "\x89PNG\r\n\x1a\n" + "\x00\x00\x00\rIHDR"
	webpBytes = "RIFF\x00\x00\x00\x00WEBPVP8 "
	pngData   = base64.StdEncoding.EncodeToString([]byte(pngBytes))
)

func TestImageGating(t *testing.T) {
	big := base64.StdEncoding.EncodeToString([]byte(pngBytes + strings.Repeat("\x00", filesystem.MaxMediaBytes)))
	bigSize := len(pngBytes) + filesystem.MaxMediaBytes
	unpadded := base64.RawStdEncoding.EncodeToString([]byte(pngBytes + "x"))
	image := func(mediaType, raw string) llm.Block {
		return llm.Block{Type: llm.BlockImage, Source: &llm.Source{Type: "base64", MediaType: mediaType,
			Data: base64.StdEncoding.EncodeToString([]byte(raw))}}
	}
	for _, tt := range []struct {
		name    string
		profile llm.Profile
		content Content
		want    llm.Block
	}{
		{"allowed", imagesOK, Content{Type: "image", MimeType: "image/webp", Data: base64.StdEncoding.EncodeToString([]byte(webpBytes))},
			image("image/webp", webpBytes)},
		{"unpadded base64", imagesOK, Content{Type: "image", MimeType: "image/png", Data: unpadded},
			image("image/png", pngBytes+"x")},
		{"declared type disagrees: sniffed type wins", imagesOK, Content{Type: "image", MimeType: "image/jpeg", Data: pngData},
			image("image/png", pngBytes)},
		{"model without images", llm.Profile{}, Content{Type: "image", MimeType: "image/png", Data: pngData},
			llm.TextBlock("[image omitted: image/png; the model does not take images]")},
		{"invalid base64", imagesOK, Content{Type: "image", MimeType: "image/png", Data: "not base64!!"},
			llm.TextBlock("[image omitted: image/png; the data is not valid base64]")},
		{"not an image", imagesOK, Content{Type: "image", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("hello, world"))},
			llm.TextBlock("[image omitted: image/png; the data is text/plain, not a PNG, JPEG, GIF or WebP image]")},
		{"svg is not passed", imagesOK, Content{Type: "image", MimeType: "image/svg+xml", Data: "PHN2Zz48L3N2Zz4="},
			llm.TextBlock("[image omitted: image/svg+xml; the data is text/plain, not a PNG, JPEG, GIF or WebP image]")},
		{"too large", imagesOK, Content{Type: "image", MimeType: "image/png", Data: big},
			llm.TextBlock(fmt.Sprintf("[image omitted: image/png; %d bytes, over the %d-byte limit]", bigSize, filesystem.MaxMediaBytes))},
		{"resource blob without images", llm.Profile{}, Content{Type: "resource", Resource: &ResourceContent{URI: "file:///i", MimeType: "image/png", Blob: pngData}},
			llm.TextBlock("[image omitted: image/png; the model does not take images]")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := toolResult(&CallToolResult{Content: []Content{tt.content}}, tt.profile)
			if len(got.Content) != 1 || !reflect.DeepEqual(got.Content[0], tt.want) {
				t.Fatalf("content = %+v, want %+v", got.Content, tt.want)
			}
		})
	}
}

func TestBadImageDoesNotPoisonOtherBlocks(t *testing.T) {
	got := toolResult(&CallToolResult{Content: []Content{
		{Type: "text", Text: "before"},
		{Type: "image", MimeType: "image/png", Data: "%%%"},
		{Type: "image", MimeType: "image/png", Data: pngData},
	}}, imagesOK)
	if len(got.Content) != 3 || got.Content[0].Text != "before" || got.Content[1].Type != llm.BlockText || got.Content[2].Type != llm.BlockImage {
		t.Fatalf("content = %+v, want the text, a note and the valid image", got.Content)
	}
}

func TestImagesFollowCallProfile(t *testing.T) {
	s := NewServer(Implementation{Name: "s", Version: "1"}, "")
	s.AddTool(ToolInfo{Name: "pic"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
		return &CallToolResult{Content: []Content{{Type: "image", MimeType: "image/png", Data: pngData}}}, nil
	})
	m := connectMiddleware(t, inProcessServer("srv", s))
	pic := findTool(t, m.DynamicTools(), "mcp__srv__pic")
	for _, tt := range []struct {
		profile llm.Profile
		want    llm.BlockType
	}{{imagesOK, llm.BlockImage}, {llm.Profile{}, llm.BlockText}} {
		res, err := pic.Call(t.Context(), json.RawMessage(`{}`), &tool.Call{Profile: tt.profile})
		if err != nil || len(res.Content) != 1 || res.Content[0].Type != tt.want {
			t.Fatalf("profile %+v: result = %+v, %v; want one %s block", tt.profile, res, err, tt.want)
		}
	}
}

func TestListChangedRefresh(t *testing.T) {
	s := newTestServer(nil)
	m := connectMiddleware(t, inProcessServer("srv", s))
	has := func(name string) bool { return slices.Contains(toolNames(m.DynamicTools()), name) }
	if has("mcp__srv__late") {
		t.Fatal("late tool present before it was added")
	}

	s.AddTool(ToolInfo{Name: "late"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
		return TextResult("late"), nil
	})
	waitFor(t, "mcp__srv__late added", func() bool { return has("mcp__srv__late") })
	p := llmtest.New(llmtest.Turn{Text: "done"})
	runAgent(t, m, agent.Config{Provider: p}, nil)
	var schemas []string
	for _, s := range p.Requests[0].Tools {
		schemas = append(schemas, s.Name)
	}
	if !slices.Contains(schemas, "mcp__srv__late") {
		t.Fatalf("request tools = %v, want mcp__srv__late", schemas)
	}

	s.RemoveTool("late")
	waitFor(t, "mcp__srv__late removed", func() bool { return !has("mcp__srv__late") })
}

func TestRefreshCollisionKeepsOldSet(t *testing.T) {
	s := newTestServer(nil)
	s.AddTool(ToolInfo{Name: "a_b"}, nil)
	m := connectMiddleware(t, inProcessServer("srv", s))
	before := toolNames(m.DynamicTools())
	s.AddTool(ToolInfo{Name: "a.b"}, nil) // sanitizes to a_b's name
	waitFor(t, "refresh failure reported", func() bool { return m.Status()[0].Err != nil })
	if got := toolNames(m.DynamicTools()); !slices.Equal(got, before) {
		t.Fatalf("tools = %v, want the old set %v", got, before)
	}
	if st := m.Status()[0]; !st.Connected || !strings.Contains(st.Err.Error(), "a.b") {
		t.Fatalf("status = %+v, want connected with the collision error", st)
	}
}

func TestRefreshFailureLogged(t *testing.T) {
	var buf syncBuffer
	s := newTestServer(nil)
	s.AddTool(ToolInfo{Name: "a_b"}, nil)
	m, err := Connect(context.Background(), []ServerConfig{inProcessServer("srv", s)},
		ConnectConfig{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	s.AddTool(ToolInfo{Name: "a.b"}, nil) // sanitizes to a_b's name
	waitFor(t, "refresh failure logged", func() bool {
		out := buf.String()
		return strings.Contains(out, `"msg":"mcp: tool list refresh failed"`) &&
			strings.Contains(out, `"server":"srv"`) && strings.Contains(out, `"level":"WARN"`) &&
			strings.Contains(out, "a.b")
	})
}

func TestAllowlist(t *testing.T) {
	s := NewServer(Implementation{Name: "s", Version: "1"}, "")
	for _, name := range []string{"read_a", "read_b", "write_c"} {
		s.AddTool(ToolInfo{Name: name}, nil)
	}
	cfg := inProcessServer("srv", s)
	cfg.Tools = []string{"read_*"}
	m := connectMiddleware(t, cfg)
	if got := toolNames(m.DynamicTools()); !slices.Equal(got, []string{"mcp__srv__read_a", "mcp__srv__read_b"}) {
		t.Fatalf("tools = %v, want only the read_ tools", got)
	}
	if st := m.Status(); st[0].Tools != 2 {
		t.Fatalf("status = %+v, want 2 tools", st)
	}
}

func TestDuplicateSanitizedNamesFailConnect(t *testing.T) {
	s := NewServer(Implementation{Name: "s", Version: "1"}, "")
	s.AddTool(ToolInfo{Name: "a.b"}, nil)
	s.AddTool(ToolInfo{Name: "a/b"}, nil)
	_, err := Connect(t.Context(), []ServerConfig{inProcessServer("srv", s)}, ConnectConfig{})
	if err == nil || !strings.Contains(err.Error(), "a.b") || !strings.Contains(err.Error(), "a/b") {
		t.Fatalf("err = %v, want a collision error naming both tools", err)
	}
}

func TestGatedServerLockedUntilUnlocked(t *testing.T) {
	cfg := inProcessServer("srv", newTestServer(nil))
	cfg.Gated = true
	m := connectMiddleware(t, cfg)
	echo := findTool(t, m.DynamicTools(), "mcp__srv__echo")
	if g, ok := echo.(deferred.Gater); !ok || !g.Gated() {
		t.Fatal("gated server's tools must implement deferred.Gater")
	}
	if !m.HasHiddenTools() {
		t.Fatal("HasHiddenTools = false for a gated server")
	}
	s := state.New()
	want := fmt.Sprintf(deferred.LockedText, "mcp__srv__echo")
	if a, ok := tool.As[tool.Availability](echo); !ok || a.Available(s) == nil || a.Available(s).Error() != want {
		t.Fatal("locked tool must report itself unavailable via tool.Availability")
	}
	if res := callTool(t, echo, s, `{"text":"x"}`); !res.IsError || res.Flatten() != want {
		t.Fatalf("locked call = %+v, want %q", res, want)
	}

	// Through the agent with no approver: refused, not interrupted for
	// approval.
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e", Name: "mcp__srv__echo", Input: map[string]any{"text": "x"}}}},
		llmtest.Turn{Text: "done"},
	)
	final := runAgent(t, m, agent.Config{Provider: p}, nil)
	if final.Reason != agent.ReasonCompleted || blockText(turnResults(final)[0]) != want {
		t.Fatalf("final = %+v, want completed with the locked error", final)
	}

	// With the deferred middleware the locked schemas are hidden too.
	p = llmtest.New(llmtest.Turn{Text: "done"})
	runAgent(t, m, agent.Config{Provider: p, Middleware: []agent.Middleware{mustDeferred(t, deferred.Config{})}}, nil)
	for _, schema := range p.Requests[0].Tools {
		if strings.HasPrefix(schema.Name, "mcp__srv__") {
			t.Fatalf("locked tool %s shown to the model", schema.Name)
		}
	}

	deferred.Unlock(s, "mcp__srv__*")
	if res := callTool(t, echo, s, `{"text":"x"}`); res.IsError || res.Flatten() != "x" {
		t.Fatalf("unlocked call = %+v", res)
	}
}

func TestDeferredServer(t *testing.T) {
	cfg := inProcessServer("srv", newTestServer(nil))
	cfg.Defer = true
	m := connectMiddleware(t, cfg)
	echo := findTool(t, m.DynamicTools(), "mcp__srv__echo")
	if d, ok := echo.(deferred.Deferrer); !ok || !d.Deferred() {
		t.Fatal("deferred server's tools must implement deferred.Deferrer")
	}
	if g, ok := echo.(deferred.Gater); ok && g.Gated() {
		t.Fatal("deferred server's tools must not be gated")
	}
	if !m.HasHiddenTools() {
		t.Fatal("HasHiddenTools = false for a deferred server")
	}
	if res := callTool(t, echo, state.New(), `{"text":"x"}`); res.IsError {
		t.Fatalf("deferred call = %+v, want it to run", res)
	}
	plain := connectMiddleware(t, inProcessServer("plain", newTestServer(nil)))
	if plain.HasHiddenTools() {
		t.Fatal("HasHiddenTools = true without Defer or Gated")
	}
}

func TestMCPToolAsksByDefault(t *testing.T) {
	s := newTestServer(nil)
	s.AddTool(ToolInfo{Name: "peek", Annotations: &ToolAnnotations{ReadOnlyHint: boolPtr(true)}},
		func(context.Context, json.RawMessage) (*CallToolResult, error) { return TextResult("peeked"), nil })
	call := func(name string) *llmtest.Scripted {
		return llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c", Name: name, Input: map[string]any{}}}},
			llmtest.Turn{Text: "done"},
		)
	}

	untrusted := connectMiddleware(t, inProcessServer("srv", s))
	for _, name := range []string{"mcp__srv__echo", "mcp__srv__peek"} {
		if final := runAgent(t, untrusted, agent.Config{Provider: call(name)}, nil); final.Reason != agent.ReasonInterrupted {
			t.Fatalf("%s: final = %+v, want an approval interrupt", name, final)
		}
	}

	cfg := inProcessServer("trusted", s)
	cfg.TrustReadOnlyHints = true
	trusted := connectMiddleware(t, cfg)
	peek := findTool(t, trusted.DynamicTools(), "mcp__trusted__peek")
	if !isReadOnly(peek, nil) || !isConcurrent(peek, nil) {
		t.Fatal("trusted readOnlyHint must make the tool read-only and concurrent")
	}
	if isReadOnly(findTool(t, trusted.DynamicTools(), "mcp__trusted__echo"), nil) {
		t.Fatal("a tool without the hint must not be read-only")
	}
	final := runAgent(t, trusted, agent.Config{Provider: call("mcp__trusted__peek")}, nil)
	if final.Reason != agent.ReasonCompleted || blockText(turnResults(final)[0]) != "peeked" {
		t.Fatalf("final = %+v, want the hinted tool to run without approval", final)
	}
}

func TestCallAfterServerDied(t *testing.T) {
	m := connectMiddleware(t, ServerConfig{
		Name:    "crashy",
		Command: os.Args[0],
		Args:    []string{"-test.run=^$"},
		Env:     map[string]string{helperEnv: "crash", "GORACE": "atexit_sleep_ms=0"},
	})
	tools := m.DynamicTools()
	s := state.New()
	if len(tools) == 0 {
		t.Fatal("no tools before the crash")
	}
	if res := callTool(t, findTool(t, tools, "mcp__crashy__crash"), s, `{}`); !res.IsError {
		t.Fatalf("crash call = %+v, want an error result", res)
	}
	res := callTool(t, findTool(t, tools, "mcp__crashy__echo"), s, `{"text":"x"}`)
	if !res.IsError || !strings.HasPrefix(res.Flatten(), "MCP server crashy failed: ") {
		t.Fatalf("call after crash = %+v, want the server failure", res)
	}
	if st := m.Status()[0]; st.Connected || st.Err == nil || st.Tools != 0 {
		t.Fatalf("status = %+v, want disconnected with an error", st)
	}
	if got := m.DynamicTools(); len(got) != 0 {
		t.Fatalf("DynamicTools = %v, want none from a dead server", toolNames(got))
	}
}

// listGate wraps a Transport: every tools/list request after the first
// waits for release (or its ctx), so a test can hold a refresh in flight.
type listGate struct {
	Transport
	release  chan struct{}
	lists    atomic.Int32 // tools/list requests seen
	returned atomic.Int32 // held requests that have returned
}

func newListGate(t Transport) *listGate { return &listGate{Transport: t, release: make(chan struct{})} }

func (g *listGate) Send(ctx context.Context, msg json.RawMessage) error {
	var m struct{ Method string }
	if json.Unmarshal(msg, &m) == nil && m.Method == "tools/list" && g.lists.Add(1) > 1 {
		defer g.returned.Add(1)
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.Transport.Send(ctx, msg)
}

func TestRefreshCoalescesNotices(t *testing.T) {
	srv := newTestServer(nil)
	gate := newListGate(InProcess(srv))
	m := connectMiddleware(t, ServerConfig{Name: "srv", Transport: gate})
	conn := m.servers[0]

	srv.AddTool(ToolInfo{Name: "x"}, nil)
	waitFor(t, "first refresh in flight", func() bool { return gate.lists.Load() == 2 })
	srv.AddTool(ToolInfo{Name: "y"}, nil)
	srv.AddTool(ToolInfo{Name: "z"}, nil)
	waitFor(t, "notices marked dirty", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return conn.dirty
	})
	close(gate.release)
	waitFor(t, "y and z listed", func() bool {
		names := toolNames(m.DynamicTools())
		return slices.Contains(names, "mcp__srv__y") && slices.Contains(names, "mcp__srv__z")
	})
	waitFor(t, "refresh loop done", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return !conn.refreshing
	})
	if n := gate.lists.Load(); n != 3 {
		t.Fatalf("tools/list requests = %d, want 3 (connect, refresh, one more pass for both notices)", n)
	}
}

func TestCloseWaitsForRefresh(t *testing.T) {
	srv := newTestServer(nil)
	gate := newListGate(InProcess(srv))
	m, err := Connect(t.Context(), []ServerConfig{{Name: "srv", Transport: gate}}, ConnectConfig{})
	if err != nil {
		t.Fatal(err)
	}
	srv.AddTool(ToolInfo{Name: "x"}, nil)
	waitFor(t, "refresh in flight", func() bool { return gate.lists.Load() == 2 })
	_ = m.Close(t.Context())
	if gate.returned.Load() != 1 {
		t.Fatal("Close returned before the in-flight refresh ended")
	}
}

func TestListChangedIgnoredWithoutToolsCapability(t *testing.T) {
	ft := newFakeTransport(func(msg map[string]any) []string { return initializeReply(msg, ProtocolVersion) })
	m := connectMiddleware(t, ServerConfig{Name: "fake", Transport: ft})
	ft.push(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	// answered after the notice was handled
	ft.push(`{"jsonrpc":"2.0","method":"ping","id":"sync"}`)
	ft.waitSent(t, func(msg map[string]any) bool { return msg["id"] == "sync" })
	conn := m.servers[0]
	conn.mu.Lock()
	refreshing := conn.refreshing
	conn.mu.Unlock()
	if refreshing || sentCount(ft, "tools/list") != 0 {
		t.Fatal("list_changed from a server without the tools capability started a refresh")
	}
}

// sentCount counts the messages f was sent that call method.
func sentCount(f *fakeTransport, method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, msg := range f.sent {
		if msg["method"] == method {
			n++
		}
	}
	return n
}

// isReadOnly reports whether tool.As finds t read-only for input in.
func isReadOnly(t tool.Tool, in json.RawMessage) bool {
	r, ok := tool.As[tool.ReadOnlyer](t)
	return ok && r.ReadOnly(in)
}

// isConcurrent reports whether tool.As finds t concurrency-safe for input in.
func isConcurrent(t tool.Tool, in json.RawMessage) bool {
	c, ok := tool.As[tool.ConcurrencySafer](t)
	return ok && c.ConcurrencySafe(in)
}

func TestAllowlistPatterns(t *testing.T) {
	tests := []struct {
		name  string
		tools []string
		tool  string
		want  bool
	}{
		{"empty list admits all", nil, "anything", true},
		{"prefix", []string{"read_*"}, "read_a", true},
		{"prefix miss", []string{"read_*"}, "xread_a", false},
		{"suffix", []string{"*_read"}, "a_read", true},
		{"question mark", []string{"get_?"}, "get_a", true},
		{"question mark needs one", []string{"get_?"}, "get_", false},
		{"literal with dot", []string{"a.b"}, "axb", false},
		{"star crosses slash", []string{"a*"}, "a/b", true},
		{"slash is literal", []string{"a/b"}, "a/b", true},
		{"question mark crosses slash", []string{"a?b"}, "a/b", true},
		{"case sensitive", []string{"read"}, "READ", false},
		{"any of several", []string{"x", "read_*"}, "read_z", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &serverConn{cfg: ServerConfig{Tools: tt.tools}}
			if got := s.allowed(tt.tool); got != tt.want {
				t.Errorf("allowed(%q) with %q = %v, want %v", tt.tool, tt.tools, got, tt.want)
			}
		})
	}
}
