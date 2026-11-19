package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

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
	if r, ok := titled.(tool.RawInputer); !ok || !r.AcceptsRawInput() {
		t.Fatal("MCP tools must accept raw input")
	}
	if titled.ReadOnly(nil) || titled.ConcurrencySafe(nil) {
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
	image := llm.Block{Type: llm.BlockImage, Source: &llm.Source{Type: "base64", MediaType: "image/png", Data: "aW1n"}}
	for _, tc := range []struct {
		name string
		in   CallToolResult
		want tool.Result
	}{
		{"text", CallToolResult{Content: []Content{{Type: "text", Text: "hello"}}},
			tool.Text("hello")},
		{"image", CallToolResult{Content: []Content{{Type: "image", Data: "aW1n", MimeType: "image/png"}}},
			tool.Result{Content: []llm.Block{image}}},
		{"audio", CallToolResult{Content: []Content{{Type: "audio", Data: "YXU=", MimeType: "audio/wav"}}},
			tool.Text("[audio content omitted: audio/wav]")},
		{"resource text", CallToolResult{Content: []Content{{Type: "resource", Resource: &ResourceContent{URI: "file:///a", Text: "body"}}}},
			tool.Text("Resource file:///a:\nbody")},
		{"resource blob image", CallToolResult{Content: []Content{{Type: "resource", Resource: &ResourceContent{URI: "file:///i", MimeType: "image/png", Blob: "aW1n"}}}},
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
		t.Run(tc.name, func(t *testing.T) {
			if got := toolResult(&tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("result = %+v, want %+v", got, tc.want)
			}
		})
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
	_, err := Connect(context.Background(), []ServerConfig{inProcessServer("srv", s)})
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
	if a, ok := echo.(tool.Availability); !ok || a.Available(s) == nil || a.Available(s).Error() != want {
		t.Fatal("locked tool must report itself unavailable via tool.Availability")
	}
	if res := callTool(t, echo, s, `{"text":"x"}`); !res.IsError || res.Flatten() != want {
		t.Fatalf("locked call = %+v, want %q", res, want)
	}

	// Through the agent with no approver: refused, not interrupted for approval.
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
	runAgent(t, m, agent.Config{Provider: p, Middleware: []agent.Middleware{deferred.New(deferred.Config{})}}, nil)
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
	if !peek.ReadOnly(nil) || !peek.ConcurrencySafe(nil) {
		t.Fatal("trusted readOnlyHint must make the tool read-only and concurrent")
	}
	if findTool(t, trusted.DynamicTools(), "mcp__trusted__echo").ReadOnly(nil) {
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
	if res := callTool(t, findTool(t, tools, "mcp__crashy__crash"), s, `{}`); !res.IsError {
		t.Fatalf("crash call = %+v, want an error result", res)
	}
	res := callTool(t, findTool(t, tools, "mcp__crashy__echo"), s, `{"text":"x"}`)
	if !res.IsError || !strings.HasPrefix(res.Flatten(), "MCP server crashy failed: ") {
		t.Fatalf("call after crash = %+v, want the server failure", res)
	}
	if st := m.Status()[0]; st.Connected || st.Err == nil {
		t.Fatalf("status = %+v, want disconnected with an error", st)
	}
}
