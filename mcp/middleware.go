package mcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Limits of the agent middleware. Everything a server sends (tool names,
// descriptions, schemas, instructions) is untrusted and capped.
const (
	// MaxToolNameLen is the longest tool name the middleware registers; see
	// ToolName.
	MaxToolNameLen = 64
	// MaxServerNameLen is the longest server name, short enough that a
	// shortened tool name always keeps the whole server prefix.
	MaxServerNameLen = 32
	// DefaultMaxInstructions caps, in bytes (cut on a rune boundary), the
	// server instructions placed in the system prompt.
	DefaultMaxInstructions = 2000
	// MaxDescriptionBytes caps a tool's description (cut on a rune boundary).
	MaxDescriptionBytes = 4096
	// MaxSchemaBytes caps a tool's input schema, measured as JSON; a larger
	// one is replaced by {"type":"object"}.
	MaxSchemaBytes = 64 << 10
	// ToolListResources lists the resources of the connected servers.
	ToolListResources = "list_mcp_resources"
	// ToolReadResource reads one resource of a connected server.
	ToolReadResource = "read_mcp_resource"
)

// ServerConfig configures one MCP server of the middleware. Exactly one of
// Command, URL and Transport must be set.
type ServerConfig struct {
	// Name identifies the server; required. It uses only [A-Za-z0-9_-],
	// contains no "__", neither starts nor ends with '_', is at most
	// MaxServerNameLen bytes and is unique among the servers, so the tool
	// name prefix "mcp__<name>__" belongs to this server alone.
	Name string

	// Command runs the server as a child process speaking stdio; see
	// StdioConfig for Args, Env, Dir and InheritEnv. Empty → not stdio.
	Command string
	// Args are the command's arguments; nil → none.
	Args []string
	// Env holds variables set for the command, over the inherited ones;
	// nil → none.
	Env map[string]string
	// Dir is the command's working directory; empty → the current one.
	Dir string
	// InheritEnv passes the whole host environment to the command instead
	// of the safe allowlist; false → allowlist only. It may expose secrets.
	InheritEnv bool

	// URL is the server's streamable HTTP endpoint; empty → not HTTP.
	URL string
	// Headers are sent with every HTTP request; nil → none.
	Headers map[string]string
	// BearerToken, when set, is sent as "Authorization: Bearer <token>"
	// unless Headers sets Authorization; empty → none.
	BearerToken string

	// Transport is a custom transport (such as InProcess), used as is;
	// nil → none. Connect takes ownership: it is closed with the middleware,
	// or by Connect when it fails.
	Transport Transport

	// Tools are globs (internal/glob syntax) over the server's own tool
	// names; only matching tools are offered. Empty → all tools.
	Tools []string
	// Defer marks the server's tools deferred.Deferrer: offered by name
	// until searched for. false → schemas always shown.
	Defer bool
	// Gated marks the server's tools deferred.Gater: hidden and refused
	// until unlocked (deferred.Unlock with "mcp__<name>__*", as a skill
	// listing the server in mcp-servers does). The server's resources and
	// instructions are withheld while it is locked; they unlock only when
	// the unlock set matches the bare prefix "mcp__<name>__", as that
	// pattern does; unlocking single tools of the server does not unlock
	// them. false → not gated.
	Gated bool
	// Optional records a connect failure in Status instead of failing
	// Connect; false → the failure fails Connect.
	Optional bool
	// IgnoreInstructions drops the server's instructions instead of
	// placing them in the system prompt; false → they are placed (see
	// Middleware).
	IgnoreInstructions bool
	// TrustReadOnlyHints makes a tool annotated readOnlyHint=true read-only
	// and concurrency-safe, so it runs without approval in the default
	// permission mode. false → every tool asks, whatever its hints say:
	// hints are untrusted.
	TrustReadOnlyHints bool

	// ConnectTimeout bounds starting the server, the handshake and the first
	// tool listing; 0 → DefaultConnectTimeout.
	ConnectTimeout time.Duration
	// RequestTimeout bounds tool-list refreshes and resource requests;
	// 0 → DefaultRequestTimeout.
	RequestTimeout time.Duration
	// CallTimeout bounds each tool call; 0 → DefaultCallTimeout.
	CallTimeout time.Duration
}

// ServerStatus describes one configured server.
type ServerStatus struct {
	// Name is the server's name.
	Name string
	// Connected is true while the connection is up.
	Connected bool
	// Tools is how many tools the server currently offers to the agent.
	Tools int
	// Err is why the server is not connected (failed to connect, or the
	// connection ended), or the last failed tool-list refresh of a connected
	// server; nil otherwise.
	Err error
}

// Middleware offers the tools of MCP servers to an agent. Each server tool
// is a tool named ToolName(server, tool) (a DynamicToolProvider, so a
// server's list_changed notification changes the set from the next turn);
// the resource tools are static (ToolProvider); WrapModel appends the
// servers' instructions to the system prompt.
//
// What a server sends is untrusted. Its instructions go into the system
// prompt framed as <mcp_server_instructions server="<name>">, capped at
// DefaultMaxInstructions, with any tag of that name inside them neutralized
// so they cannot end the frame or pose as another server; they can still
// steer the agent, so connect only servers you trust or set
// ServerConfig.IgnoreInstructions. Its tools ask for approval by default
// (see ServerConfig.TrustReadOnlyHints), and its images reach the model only
// when the model takes images. It is safe for concurrent use by many runs.
type Middleware struct {
	servers   []*serverConn
	resources []tool.Tool

	closeOnce sync.Once
	closeErr  error
}

// Connect validates servers and connects to all of them in parallel. When a
// server that is not Optional fails, it stops the connections still in
// progress, closes every server and returns the error (joined with those of
// other servers that failed on their own).
func Connect(ctx context.Context, servers []ServerConfig) (*Middleware, error) {
	if err := validateServers(servers); err != nil {
		for _, cfg := range servers {
			if cfg.Transport != nil {
				_ = cfg.Transport.Close()
			}
		}
		return nil, err
	}
	m := &Middleware{}
	for _, cfg := range servers {
		m.servers = append(m.servers, newServerConn(cfg))
	}
	// abort stops the other connections once a required server failed.
	connectCtx, abort := context.WithCancel(ctx)
	defer abort()
	errs := make([]error, len(m.servers))
	var wg sync.WaitGroup
	for i, s := range m.servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.connect(connectCtx)
			if err == nil {
				return
			}
			s.err = fmt.Errorf("mcp: server %s: %w", s.cfg.Name, err)
			aborted := connectCtx.Err() != nil && ctx.Err() == nil && errors.Is(err, context.Canceled)
			if !s.cfg.Optional && !aborted {
				errs[i] = s.err
				abort()
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		_ = m.Close()
		return nil, err
	}
	if slices.ContainsFunc(m.servers, func(s *serverConn) bool { return s.client != nil && s.client.HasResources() }) {
		m.resources = []tool.Tool{m.listResourcesTool(), m.readResourceTool()}
	}
	return m, nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return "mcp" }

// DynamicTools implements agent.DynamicToolProvider: the current tools of
// the servers whose connection is up, in server order. A server whose
// connection ended offers none.
func (m *Middleware) DynamicTools() []tool.Tool {
	var tools []tool.Tool
	for _, s := range m.servers {
		if s.connected() {
			tools = append(tools, s.currentTools()...)
		}
	}
	return tools
}

// Tools implements agent.ToolProvider: ToolListResources and
// ToolReadResource when a server offered resources at Connect, else none.
func (m *Middleware) Tools() []tool.Tool { return slices.Clone(m.resources) }

// Status reports every configured server, in configuration order.
func (m *Middleware) Status() []ServerStatus {
	out := make([]ServerStatus, len(m.servers))
	for i, s := range m.servers {
		out[i] = s.status()
	}
	return out
}

// HasHiddenTools reports whether any server is configured with Defer or
// Gated, whose tools need the deferred middleware to be found or unlocked.
func (m *Middleware) HasHiddenTools() bool {
	return slices.ContainsFunc(m.servers, func(s *serverConn) bool { return s.cfg.Defer || s.cfg.Gated })
}

// Close closes every server connection and waits for pending tool-list
// refreshes. It is idempotent.
func (m *Middleware) Close() error {
	m.closeOnce.Do(func() {
		var errs []error
		for _, s := range m.servers {
			errs = append(errs, s.close())
		}
		m.closeErr = errors.Join(errs...)
	})
	return m.closeErr
}

// instructionsTag is the element that frames a server's instructions in
// the system prompt.
const instructionsTag = "mcp_server_instructions"

// neutralizeInstructions escapes every tag of instructionsTag inside a
// server's instructions.
var neutralizeInstructions = promptsafe.NewTagNeutralizer(instructionsTag)

// WrapModel implements agent.ModelWrapper: it appends one segment holding
// the framed instructions of every connected server that has instructions,
// does not ignore them and is not locked in the run, after the host's
// segments. No segment when there are none.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	var frames []string
	for _, s := range m.servers {
		if s.cfg.IgnoreInstructions || !s.connected() || s.locked(req.State) {
			continue
		}
		if text := s.client.Instructions(); text != "" {
			text, _ = promptsafe.CapBytes(text, DefaultMaxInstructions)
			frames = append(frames, fmt.Sprintf("<%s server=%q>\n%s\n</%s>", instructionsTag, s.cfg.Name, neutralizeInstructions(text), instructionsTag))
		}
	}
	if len(frames) > 0 {
		req.Request.System = append(slices.Clone(req.Request.System), strings.Join(frames, "\n"))
	}
	return next(ctx, req)
}

// serverNamePattern is the alphabet of a server name, which must neither
// start nor end with '_'.
var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9-]([A-Za-z0-9_-]*[A-Za-z0-9-])?$`)

// validateServers checks every server's name and transport choice.
func validateServers(servers []ServerConfig) error {
	seen := make(map[string]bool, len(servers))
	for _, cfg := range servers {
		if err := validateName(cfg.Name); err != nil {
			return err
		}
		if seen[cfg.Name] {
			return fmt.Errorf("mcp: duplicate server name %q", cfg.Name)
		}
		seen[cfg.Name] = true
		set := 0
		for _, ok := range []bool{cfg.Command != "", cfg.URL != "", cfg.Transport != nil} {
			if ok {
				set++
			}
		}
		if set != 1 {
			return fmt.Errorf("mcp: server %q: set exactly one of Command, URL and Transport", cfg.Name)
		}
	}
	return nil
}

// validateName reports why name cannot name a server; see ServerConfig.Name.
func validateName(name string) error {
	switch {
	case name == "":
		return errors.New("mcp: missing server name")
	case len(name) > MaxServerNameLen:
		return fmt.Errorf("mcp: server name %q is longer than %d bytes", name, MaxServerNameLen)
	case !serverNamePattern.MatchString(name) || strings.Contains(name, "__"):
		return fmt.Errorf("mcp: invalid server name %q: use [A-Za-z0-9_-], no \"__\", no leading or trailing '_'", name)
	}
	return nil
}
