package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// refreshWait bounds how long a test waits for a list_changed refresh.
const refreshWait = 2 * time.Second

// boolPtr returns a pointer to b, for optional annotation hints.
func boolPtr(b bool) *bool { return &b }

// connectMiddleware connects servers and closes the middleware when the test
// ends.
func connectMiddleware(t *testing.T, servers ...ServerConfig) *Middleware {
	t.Helper()
	m, err := Connect(context.Background(), servers)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// inProcessServer configures the server named name served in process by s.
func inProcessServer(name string, s *Server) ServerConfig {
	return ServerConfig{Name: name, Transport: InProcess(s)}
}

// allowAll approves every call.
func allowAll(context.Context, permission.Request, string) permission.Decision {
	return permission.Allowed()
}

// runAgent runs an agent with m and cfg's other settings to completion and
// returns its final event.
func runAgent(t *testing.T, m *Middleware, cfg agent.Config, s *state.State) *agent.Final {
	t.Helper()
	cfg.Middleware = append(cfg.Middleware, m)
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var final *agent.Final
	for ev, err := range a.Run(context.Background(), agent.Input{Messages: []llm.Message{llm.UserText("go")}, State: s}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatal("run produced no final event")
	}
	return final
}

// turnResults returns the tool_result blocks answering the first model turn.
func turnResults(final *agent.Final) []llm.Block {
	return final.State.Messages()[2].Content
}

func blockText(b llm.Block) string {
	var parts []string
	for _, c := range b.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "")
}

func toolNames(tools []tool.Tool) []string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}

func findTool(t *testing.T, tools []tool.Tool, name string) tool.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("tool %s not in %v", name, toolNames(tools))
	return nil
}

// callTool calls tl directly in the run with State s.
func callTool(t *testing.T, tl tool.Tool, s *state.State, input string) tool.Result {
	t.Helper()
	res, err := tl.Call(context.Background(), json.RawMessage(input), &tool.Call{State: s})
	if err != nil {
		t.Fatalf("%s: %v", tl.Name(), err)
	}
	return res
}

// waitFor polls cond until it holds or refreshWait passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(refreshWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s not within %v", what, refreshWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestToolsExposedAndCalled(t *testing.T) {
	m := connectMiddleware(t, inProcessServer("srv", newTestServer(nil)))
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "e", Name: "mcp__srv__echo", Input: map[string]any{"text": "hi"}},
			{ID: "f", Name: "mcp__srv__fail", Input: map[string]any{}},
		}},
		llmtest.Turn{Text: "done"},
	)
	final := runAgent(t, m, agent.Config{Provider: p, Approver: allowAll}, nil)

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	var echo *llm.ToolSchema
	for i, s := range p.Requests[0].Tools {
		if s.Name == "mcp__srv__echo" {
			echo = &p.Requests[0].Tools[i]
		}
	}
	if echo == nil {
		t.Fatalf("request tools = %+v, want mcp__srv__echo", p.Requests[0].Tools)
	}
	want := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}
	if !reflect.DeepEqual(echo.InputSchema, want) || echo.Description != "Echoes text." {
		t.Fatalf("echo schema = %+v", echo)
	}
	results := turnResults(final)
	if results[0].IsError || blockText(results[0]) != "hi" {
		t.Fatalf("echo result = %+v", results[0])
	}
	if !results[1].IsError || blockText(results[1]) != "it broke" {
		t.Fatalf("fail result = %+v, want the server's error", results[1])
	}
}

func TestResourceTools(t *testing.T) {
	locked := inProcessServer("locked", newTestServer(nil))
	locked.Gated = true
	bare := NewServer(Implementation{Name: "bare", Version: "1"}, "")
	m := connectMiddleware(t, inProcessServer("srv", newTestServer(nil)), locked, inProcessServer("bare", bare))
	tools := m.Tools()
	if got := toolNames(tools); !slices.Equal(got, []string{ToolListResources, ToolReadResource}) {
		t.Fatalf("Tools = %v", got)
	}
	list, read := tools[0], tools[1]
	for _, tl := range tools {
		if !tl.ReadOnly(nil) || !tl.ConcurrencySafe(nil) {
			t.Fatalf("%s must be read-only and concurrent", tl.Name())
		}
	}
	s := state.New()

	if got := callTool(t, list, s, `{}`).Flatten(); got != "srv | file:///readme | readme | text/plain | " {
		t.Fatalf("list = %q, want only srv's resource", got)
	}
	if res := callTool(t, list, s, `{"server":"nope"}`); !res.IsError {
		t.Fatalf("list of an unknown server = %+v, want an error", res)
	}
	if res := callTool(t, read, s, `{"server":"srv","uri":"file:///readme"}`); res.IsError || res.Flatten() != "Resource file:///readme:\nhello" {
		t.Fatalf("read = %+v", res)
	}
	for _, in := range []string{`{"server":"locked","uri":"file:///readme"}`, `{"server":"nope","uri":"x"}`, `{"server":"srv","uri":"file:///missing"}`} {
		if res := callTool(t, read, s, in); !res.IsError {
			t.Fatalf("read %s = %+v, want an error", in, res)
		}
	}

	deferred.Unlock(s, "mcp__locked__*")
	if got := callTool(t, list, s, `{"server":"locked"}`).Flatten(); got != "locked | file:///readme | readme | text/plain | " {
		t.Fatalf("list after unlock = %q", got)
	}

	none := connectMiddleware(t, inProcessServer("bare2", bare))
	if got := none.Tools(); len(got) != 0 {
		t.Fatalf("Tools = %v, want none without resources", toolNames(got))
	}
}

func TestServerInstructionsInPrompt(t *testing.T) {
	long := NewServer(Implementation{Name: "long", Version: "1"}, strings.Repeat("i", DefaultMaxInstructions+50))
	locked := inProcessServer("locked", newTestServer(nil))
	locked.Gated = true
	silent := NewServer(Implementation{Name: "silent", Version: "1"}, "")
	m := connectMiddleware(t, inProcessServer("srv", newTestServer(nil)), inProcessServer("long", long), locked, inProcessServer("silent", silent))

	p := llmtest.New(llmtest.Turn{Text: "done"})
	runAgent(t, m, agent.Config{Provider: p, System: []string{"host"}}, nil)
	system := p.Requests[0].System
	want := "MCP server srv: use the echo tool\nMCP server long: " + strings.Repeat("i", DefaultMaxInstructions)
	if len(system) != 2 || system[0] != "host" || system[1] != want {
		t.Fatalf("system = %q, want host then %q", system, want)
	}

	s := state.New()
	deferred.Unlock(s, "mcp__locked__*")
	p = llmtest.New(llmtest.Turn{Text: "done"})
	runAgent(t, m, agent.Config{Provider: p}, s)
	if seg := p.Requests[0].System[0]; !strings.HasSuffix(seg, "\nMCP server locked: use the echo tool") {
		t.Fatalf("segment = %q, want the unlocked server's instructions", seg)
	}

	p = llmtest.New(llmtest.Turn{Text: "done"})
	quiet := connectMiddleware(t, inProcessServer("quiet", silent))
	runAgent(t, quiet, agent.Config{Provider: p, System: []string{"host"}}, nil)
	if system := p.Requests[0].System; len(system) != 1 {
		t.Fatalf("system = %q, want no segment without instructions", system)
	}
}

// closeRecorder is a Transport that records whether it was closed.
type closeRecorder struct {
	Transport
	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return c.Transport.Close()
}

func TestConnectFailureClosesOthers(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-server")
	rec := &closeRecorder{Transport: InProcess(newTestServer(nil))}
	_, err := Connect(context.Background(), []ServerConfig{
		{Name: "good", Transport: rec},
		{Name: "bad", Command: missing},
	})
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v, want the bad server's failure", err)
	}
	if !rec.closed.Load() {
		t.Fatal("the connected server was not closed")
	}

	m := connectMiddleware(t,
		inProcessServer("good", newTestServer(nil)),
		ServerConfig{Name: "bad", Command: missing, Optional: true},
	)
	st := m.Status()
	if len(st) != 2 || !st[0].Connected || st[0].Tools != 4 || st[0].Err != nil {
		t.Fatalf("status = %+v, want good connected with 4 tools", st)
	}
	if st[1].Name != "bad" || st[1].Connected || st[1].Err == nil {
		t.Fatalf("status = %+v, want bad not connected with its error", st)
	}
	if got := len(m.DynamicTools()); got != 4 {
		t.Fatalf("DynamicTools = %d tools, want 4", got)
	}
}

func TestConnectOverHTTP(t *testing.T) {
	handler := newTestServer(nil).Handler(HandlerOptions{})
	var auth atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close) // runs after the middleware closes its streams
	m := connectMiddleware(t, ServerConfig{Name: "web", URL: ts.URL, BearerToken: "tok"})
	res := callTool(t, findTool(t, m.DynamicTools(), "mcp__web__echo"), state.New(), `{"text":"over http"}`)
	if res.IsError || res.Flatten() != "over http" {
		t.Fatalf("call = %+v", res)
	}
	if got, _ := auth.Load().(string); got != "Bearer tok" {
		t.Fatalf("Authorization = %q, want the bearer token", got)
	}
}

func TestServerConfigValidation(t *testing.T) {
	tr := func() Transport { return InProcess(newTestServer(nil)) }
	for _, tc := range []struct {
		name    string
		servers []ServerConfig
	}{
		{"empty name", []ServerConfig{{Command: "x"}}},
		{"bad char", []ServerConfig{{Name: "a b", Command: "x"}}},
		{"double underscore", []ServerConfig{{Name: "a__b", Command: "x"}}},
		{"leading underscore", []ServerConfig{{Name: "_a", Command: "x"}}},
		{"trailing underscore", []ServerConfig{{Name: "a_", Command: "x"}}},
		{"too long", []ServerConfig{{Name: strings.Repeat("a", MaxServerNameLen+1), Command: "x"}}},
		{"duplicate", []ServerConfig{{Name: "a", Command: "x"}, {Name: "a", Command: "y"}}},
		{"no transport", []ServerConfig{{Name: "a"}}},
		{"command and url", []ServerConfig{{Name: "a", Command: "x", URL: "http://h"}}},
		{"url and transport", []ServerConfig{{Name: "a", URL: "http://h", Transport: tr()}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if m, err := Connect(context.Background(), tc.servers); err == nil {
				_ = m.Close()
				t.Fatal("Connect succeeded, want a validation error")
			}
		})
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	m, err := Connect(context.Background(), []ServerConfig{inProcessServer("srv", newTestServer(nil))})
	if err != nil {
		t.Fatal(err)
	}
	if m.Name() != "mcp" {
		t.Fatalf("Name = %q", m.Name())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
