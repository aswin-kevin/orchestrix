// Package deferred keeps tool schemas out of the model request until they
// are needed, and locks gated tools until something unlocks them.
//
// A deferred tool (Deferrer, Defer or a Config.Tools pattern) is offered by
// name only: the system prompt lists it, search_tools finds it and returns
// its schema, and from the next turn on its schema is part of the request.
// call_tool calls any tool by name with an input object. A gated tool
// (Gater, Gate) is hidden, unsearchable and refused until Unlock names it,
// for example when a skill that provides it is loaded.
//
// Both kinds share one per-run unlock set, UnlockedKey; search_tools also
// records the names it unlocked in a second private key, read with Searched,
// so a middleware that relocks its own grants can leave those alone. Nothing
// here weakens a permission check: call_tool is a tool.Dispatcher, so the agent loop runs
// its target through the same permission rules, mode, approvals, hitl and
// hooks as a direct call, and the Defer and Gate wrappers forward every
// permission-relevant interface of the tool they wrap.
package deferred

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Tool names and limits.
const (
	// ToolSearch is the tool that finds deferred tools and loads their
	// schemas.
	ToolSearch = "search_tools"
	// ToolCall is the name of the tool that calls another tool by name. It
	// is a tool name, like ToolSearch and the other packages' Tool…
	// constants, not a type; the call of a tool in a run is agent.ToolCall.
	ToolCall = "call_tool"
	// DefaultMaxResults is how many tools search_tools returns when neither
	// the model nor Config.MaxResults says otherwise.
	DefaultMaxResults = 5
	// MaxResults is the most tools one search returns, whatever is asked.
	MaxResults = 20
	// DefaultMaxListed is how many deferred tool names the prompt lists;
	// the rest are summarized as "+N more".
	DefaultMaxListed = 50
)

// Config configures the deferred middleware.
type Config struct {
	// Tools are name patterns (glob: "*" and "?") of tools to defer in
	// addition to the tools implementing Deferrer; nil defers only those.
	// search_tools and call_tool are never deferred.
	Tools []string
	// MaxResults is how many tools a search returns when the model does not
	// say; 0 means DefaultMaxResults, and it is capped at MaxResults.
	MaxResults int
	// Prompt introduces the list of deferred tools in the system prompt.
	// nil uses DefaultPrompt; a pointer to "" disables the segment.
	Prompt *string
}

// middleware is the deferred middleware. It is immutable after New, so it is
// safe to share between concurrent runs; per-run data lives in UnlockedKey.
type middleware struct {
	patterns   []string
	maxResults int
	prompt     string
}

// New returns the deferred middleware. It provides search_tools and
// call_tool and hides the schemas of locked tools from each model request.
func New(cfg Config) agent.Middleware {
	m := &middleware{
		patterns:   slices.Clone(cfg.Tools),
		maxResults: DefaultMaxResults,
		prompt:     DefaultPrompt,
	}
	if cfg.MaxResults > 0 {
		m.maxResults = min(cfg.MaxResults, MaxResults)
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	return m
}

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "deferred" }

// Tools implements agent.ToolProvider.
func (m *middleware) Tools() []tool.Tool {
	return []tool.Tool{m.searchTool(), callTool()}
}

// WrapModel implements agent.ModelWrapper. It drops the schemas of locked
// tools (gated and not unlocked, or deferred and not unlocked) from the
// request and, when deferred tools remain hidden, appends the prompt that
// lists them after the host's system segments.
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	info, ok := agent.Info(ctx)
	if !ok {
		return next(ctx, req)
	}
	tools := make(map[string]tool.Tool)
	for _, t := range info.Tools() {
		tools[t.Name()] = t
	}
	var shown []llm.ToolSchema
	var hidden []string // deferred, not gated: the model may search for these
	for _, schema := range req.Request.Tools {
		// A schema whose tool left the set since this turn's snapshot is
		// kept: its calls are still resolved and checked by the loop.
		t, ok := tools[schema.Name]
		switch {
		case ok && agent.ToolHidden(ctx, schema.Name):
			// Hidden by another middleware: neither shown nor listed.
		case !ok:
			shown = append(shown, schema)
		case gatedLocked(req.State, t):
		case m.deferredLocked(req.State, t):
			hidden = append(hidden, t.Name())
		default:
			shown = append(shown, schema)
		}
	}
	req.Request.Tools = shown
	if len(hidden) > 0 && m.prompt != "" {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt+"\n\n"+listing(hidden))
	}
	return next(ctx, req)
}

// listing renders the names of the hidden deferred tools for the prompt:
// sorted, at most DefaultMaxListed, then how many more there are.
func listing(names []string) string {
	names = slices.Sorted(slices.Values(names))
	var b strings.Builder
	b.WriteString("Deferred tools:")
	for _, name := range names[:min(len(names), DefaultMaxListed)] {
		b.WriteString("\n- " + name)
	}
	if more := len(names) - DefaultMaxListed; more > 0 {
		fmt.Fprintf(&b, "\n+%d more", more)
	}
	return b.String()
}

// isDeferred reports whether t is deferred: marked so itself, or named by a
// Config.Tools pattern. The middleware's own tools never are.
func (m *middleware) isDeferred(t tool.Tool) bool {
	if t.Name() == ToolSearch || t.Name() == ToolCall {
		return false
	}
	return isDeferred(t) || matchesAny(m.patterns, t.Name())
}

// deferredLocked reports whether t is deferred and not unlocked in the run
// with State s.
func (m *middleware) deferredLocked(s *state.State, t tool.Tool) bool {
	return m.isDeferred(t) && !Unlocked(s, t.Name())
}
