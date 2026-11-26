package mcp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aswin-kevin/orchestrix/internal/names"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// toolNameHashLen is how many hex digits of the full name's SHA-256 end a
// shortened tool name.
const toolNameHashLen = 8

// ToolName is the agent tool name of the tool named tool on the server named
// server: "mcp__<server>__" followed by the tool name with every rune
// outside [A-Za-z0-9_-] replaced by '_'. A name longer than MaxToolNameLen
// keeps its first MaxToolNameLen-9 bytes, then "_" and the
// first 8 hex digits of the full name's SHA-256, so it stays unique and
// deterministic.
func ToolName(server, tool string) string {
	name := names.MCPPrefix(server) + names.Sanitize(tool)
	if len(name) <= MaxToolNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:MaxToolNameLen-toolNameHashLen-1] + "_" + hex.EncodeToString(sum[:])[:toolNameHashLen]
}

// serverConn is the middleware's connection to one server and the agent
// tools built from its tool list.
type serverConn struct {
	cfg  ServerConfig
	conn ConnectConfig
	// client and err are written once by connect, before Connect returns:
	// client is nil when the server is not connected, err says why.
	client *Client
	err    error
	tools  atomic.Pointer[[]tool.Tool]

	ctx    context.Context // ends with close; bounds refreshes
	cancel context.CancelFunc

	mu sync.Mutex
	// ready is set once the first tool list is in place; refreshing while
	// a refresh goroutine runs; dirty when a list_changed notice arrived
	// that no refresh has started to handle yet.
	// hasTools is set with ready when the server declared the tools
	// capability; list_changed from a server without it is ignored.
	ready, hasTools, refreshing, dirty, closed bool
	refreshErr                                 error
	refreshes                                  sync.WaitGroup
	watcher                                    sync.WaitGroup
}

func newServerConn(cfg ServerConfig, conn ConnectConfig) *serverConn {
	s := &serverConn{cfg: cfg, conn: conn}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// connect starts the transport, initializes the client and builds the first
// tool set, all within ConnectTimeout.
func (s *serverConn) connect(ctx context.Context) error {
	t, err := s.transport()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(s.cfg.ConnectTimeout, DefaultConnectTimeout))
	defer cancel()
	client, err := NewClient(ctx, t, ClientConfig{
		Info:           s.conn.Info,
		RequestTimeout: s.requestTimeout(),
		OnNotification: s.notified,
	})
	if err != nil {
		return err // NewClient closed t
	}
	if client.HasTools() {
		tools, err := s.listTools(ctx, client)
		if err != nil {
			closeDetached(ctx, client.Close)
			return err
		}
		s.tools.Store(&tools)
	}
	s.client = client
	s.watcher.Add(1)
	go s.watch(client)
	s.mu.Lock()
	s.ready = true
	s.hasTools = client.HasTools()
	pending := s.dirty && s.hasTools
	s.dirty = false
	s.mu.Unlock()
	if pending {
		s.toolsChanged()
	}
	return nil
}

// transport opens the transport the configuration names.
func (s *serverConn) transport() (Transport, error) {
	switch {
	case s.cfg.Transport != nil:
		return s.cfg.Transport, nil
	case s.cfg.Command != "":
		return StartStdio(StdioConfig{
			Command:    s.cfg.Command,
			Args:       s.cfg.Args,
			Dir:        s.cfg.Dir,
			Env:        s.cfg.Env,
			InheritEnv: s.cfg.InheritEnv,
		})
	default:
		return DialHTTP(HTTPConfig{URL: s.cfg.URL, Headers: s.cfg.Headers, BearerToken: s.cfg.BearerToken})
	}
}

func (s *serverConn) requestTimeout() time.Duration {
	return cmp.Or(s.cfg.RequestTimeout, DefaultRequestTimeout)
}

// notified handles a server notification on the client's reader goroutine.
func (s *serverConn) notified(method string, params json.RawMessage) {
	if method == notifyToolListChanged {
		s.toolsChanged()
	}
	if s.conn.OnNotification != nil {
		s.conn.OnNotification(s.cfg.Name, method, params)
	}
}

// watch logs when client ends before the server is closed. It returns when
// the client ends or the server is closed.
func (s *serverConn) watch(client *Client) {
	defer s.watcher.Done()
	select {
	case <-client.done:
		if s.ctx.Err() == nil {
			s.conn.Logger.Warn("mcp: server connection ended", "server", s.cfg.Name, "err", client.Err())
		}
	case <-s.ctx.Done():
	}
}

// toolsChanged schedules a refresh of the tool list: it starts the refresh
// goroutine, or, while one runs (or the first list is not in yet), marks
// the list dirty so it takes one more pass. A server without the tools
// capability has no list to refresh.
func (s *serverConn) toolsChanged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed, s.ready && !s.hasTools:
	case s.refreshing || !s.ready:
		s.dirty = true
	default:
		s.refreshing = true
		s.refreshes.Add(1)
		go s.refreshLoop()
	}
}

// refreshLoop refreshes the tool list until no notice arrived meanwhile.
func (s *serverConn) refreshLoop() {
	defer s.refreshes.Done()
	for {
		s.refresh()
		s.mu.Lock()
		if !s.dirty || s.closed {
			s.refreshing = false
			s.mu.Unlock()
			return
		}
		s.dirty = false
		s.mu.Unlock()
	}
}

// refresh lists the tools again and swaps in the new set; on failure the
// old set stays and the error is kept for Status.
func (s *serverConn) refresh() {
	ctx, cancel := context.WithTimeout(s.ctx, s.requestTimeout())
	defer cancel()
	tools, err := s.listTools(ctx, s.client)
	if err == nil {
		s.tools.Store(&tools)
	} else if s.ctx.Err() == nil {
		s.conn.Logger.Warn("mcp: tool list refresh failed", "server", s.cfg.Name, "err", err)
	}
	s.mu.Lock()
	s.refreshErr = err
	s.mu.Unlock()
}

// listTools fetches the server's tools and builds the agent tools for those
// the allowlist admits. Two tools that map to the same agent tool name are
// an error, not a silent skip.
func (s *serverConn) listTools(ctx context.Context, client *Client) ([]tool.Tool, error) {
	infos, err := client.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	var tools []tool.Tool
	origin := map[string]string{} // agent tool name → server tool name
	for _, info := range infos {
		if !s.allowed(info.Name) {
			continue
		}
		if info.Name == "" {
			return nil, errors.New("server offered a tool without a name")
		}
		name := ToolName(s.cfg.Name, info.Name)
		if prev, ok := origin[name]; ok {
			return nil, fmt.Errorf("tools %q and %q both map to tool name %s", prev, info.Name, name)
		}
		origin[name] = info.Name
		tools = append(tools, s.adapter(client, name, info))
	}
	return tools, nil
}

// allowed reports whether the allowlist admits the server tool name.
func (s *serverConn) allowed(name string) bool {
	return len(s.cfg.Tools) == 0 || slices.ContainsFunc(s.cfg.Tools, func(p string) bool { return tool.MatchName(p, name) })
}

// adapter builds the agent tool named name for the server tool info.
// Locking a gated tool is left to deferred.Gate, which refuses locked calls
// and reports them unavailable before any approval is asked for.
func (s *serverConn) adapter(client *Client, name string, info ToolInfo) tool.Tool {
	spec := tool.Spec{
		Name:        name,
		Description: description(info),
		Schema:      inputSchema(info.InputSchema),
		// The server validates its own input; its schema may use features
		// the built-in validator lacks.
		RawInput: true,
		Run: func(ctx context.Context, in json.RawMessage, c *tool.Call) (tool.Result, error) {
			var profile llm.Profile
			if c != nil {
				profile = c.Profile
			}
			return s.call(ctx, client, info.Name, in, profile), nil
		},
	}
	if s.cfg.TrustReadOnlyHints && info.Annotations != nil && info.Annotations.ReadOnlyHint != nil && *info.Annotations.ReadOnlyHint {
		spec.ReadOnly = always
		spec.Concurrent = always
	}
	t := tool.Build(spec)
	if s.cfg.Defer {
		t = deferred.Defer(t)
	}
	if s.cfg.Gated {
		t = deferred.Gate(t)
	}
	return t
}

func always(json.RawMessage) bool { return true }

// call runs the server tool named name with the agent's input.
func (s *serverConn) call(ctx context.Context, client *Client, name string, in json.RawMessage, profile llm.Profile) tool.Result {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(s.cfg.CallTimeout, DefaultCallTimeout))
	defer cancel()
	if string(in) == "null" {
		in = nil
	}
	res, err := client.CallTool(ctx, name, in)
	if err != nil {
		return s.failure(err)
	}
	return toolResult(res, profile)
}

// failure is the error result of a failed request: the server's own message
// for a JSON-RPC error, else the transport failure.
func (s *serverConn) failure(err error) tool.Result {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return tool.Errorf("%s", rpcErr.Message)
	}
	return tool.Errorf("MCP server %s failed: %v", s.cfg.Name, err)
}

// currentTools returns the server's current agent tools.
func (s *serverConn) currentTools() []tool.Tool {
	if p := s.tools.Load(); p != nil {
		return *p
	}
	return nil
}

// connected reports whether the connection is up.
func (s *serverConn) connected() bool { return s.client != nil && s.client.Err() == nil }

// locked reports whether the server is gated and not unlocked in the run
// with State st: the unlock set must match the server's tool name prefix, as
// "mcp__<name>__*" does.
func (s *serverConn) locked(st *state.State) bool {
	return s.cfg.Gated && !deferred.Unlocked(st, names.MCPPrefix(s.cfg.Name))
}

func (s *serverConn) status() ServerStatus {
	st := ServerStatus{Name: s.cfg.Name, Err: s.err}
	if s.client == nil {
		return st
	}
	if st.Err = s.client.Err(); st.Err == nil {
		st.Connected = true
		st.Tools = len(s.currentTools())
		s.mu.Lock()
		st.Err = s.refreshErr
		s.mu.Unlock()
	}
	return st
}

// close ends the connection and waits for a running refresh.
func (s *serverConn) close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	var err error
	if s.client != nil {
		err = s.client.Close(ctx)
	}
	s.refreshes.Wait()
	s.watcher.Wait()
	return err
}

// description is the tool's description, its title first when it has one,
// capped at MaxDescriptionBytes.
func description(info ToolInfo) string {
	title := info.Title
	if title == "" && info.Annotations != nil {
		title = info.Annotations.Title
	}
	text := info.Description
	switch {
	case title != "" && text != "":
		text = title + ": " + text
	case title != "":
		text = title
	}
	text, _ = promptsafe.CapBytes(text, MaxDescriptionBytes)
	return text
}

// inputSchema is the server's input schema, or {"type":"object"} when it is
// missing, not an object schema, or larger than MaxSchemaBytes.
func inputSchema(schema map[string]any) map[string]any {
	if schema == nil || schema["type"] != "object" {
		return map[string]any{"type": "object"}
	}
	if raw, err := json.Marshal(schema); err != nil || len(raw) > MaxSchemaBytes {
		return map[string]any{"type": "object"}
	}
	return schema
}
