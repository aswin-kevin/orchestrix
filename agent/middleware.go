// Package agent runs the model/tool loop and defines the contracts that
// extend it.
//
// # Middleware
//
// Every optional behaviour of a run is a Middleware: a value with a Name and
// any of the hook interfaces below. The loop discovers hooks by type
// assertion, so a middleware implements only what it needs.
//
// Hooks run in this order: BeforeAgent once at the start of a run; per turn,
// BeforeModel, the model call (through every ModelWrapper), then AfterModel;
// each tool call goes through every ToolWrapper; AfterAgent once at the end.
// Before* hooks (BeforeAgent, BeforeModel) run in stack order; After* hooks
// (AfterModel, AfterAgent) run in reverse stack order, mirroring how wrappers
// unwind.
//
// Wrappers follow the usual onion model: the first middleware in the stack is
// the outermost. A wrapper may call next zero times (short-circuit), once, or
// several times (retry), and may change the request or the result.
//
// ModelRequest.Request.Messages is a view built for this call. A middleware
// may reorder, trim or rewrite it freely; the conversation held in State is
// not affected. To change the conversation itself, edit State.
//
// An Agent is shared by concurrent runs, so a middleware value must not hold
// per-run data in its own fields. Keep it in State (see state.Key), which is
// scoped to one run.
package agent

import (
	"context"
	"encoding/json"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Middleware is the base of every extension. Name identifies the middleware
// in a stack: it must be non-empty, and a user middleware whose name matches a
// built-in replaces that built-in (see Compose).
type Middleware interface {
	Name() string
}

// BeforeAgent runs once before the first turn. A returned error ends the run
// with ReasonMiddlewareError.
type BeforeAgent interface {
	BeforeAgent(ctx context.Context, s *state.State) error
}

// AfterAgent runs once after the run has finished, whatever the reason,
// unless the consumer stopped reading the run's events first.
type AfterAgent interface {
	AfterAgent(ctx context.Context, s *state.State) error
}

// BeforeModel runs before each model call, after the request has been built
// from State.
type BeforeModel interface {
	BeforeModel(ctx context.Context, s *state.State) error
}

// AfterModel runs after each successful model call with the response, before
// any tool calls it contains are executed.
type AfterModel interface {
	AfterModel(ctx context.Context, s *state.State, resp *llm.Response) error
}

// ModelWrapper wraps the model call. See the package documentation for the
// wrapping semantics.
type ModelWrapper interface {
	WrapModel(ctx context.Context, req *ModelRequest, next ModelHandler) (*llm.Response, error)
}

// ToolWrapper wraps a single tool execution. See the package documentation
// for the wrapping semantics.
type ToolWrapper interface {
	WrapTool(ctx context.Context, call *ToolCall, next ToolHandler) (tool.Result, error)
}

// ToolProvider lets a middleware contribute tools to the run, such as a
// middleware that owns a task-management tool.
type ToolProvider interface {
	Tools() []tool.Tool
}

// ModelRequest is everything a model call needs, as seen by ModelWrappers.
type ModelRequest struct {
	Provider llm.Provider
	// Request is a view: editing its Messages never touches State.
	Request llm.Request
	State   *state.State
	Profile llm.Profile
	// Turn is the zero-based index of the current model turn.
	Turn int
}

// ModelHandler performs a model call. The innermost handler talks to the
// provider; each ModelWrapper contributes one layer around it.
type ModelHandler func(ctx context.Context, req *ModelRequest) (*llm.Response, error)

// ToolCall describes one tool execution as seen by ToolWrappers. Changing
// Input before calling next changes what the tool receives.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
	Tool  tool.Tool
	State *state.State
	// Call is the per-invocation environment passed to Tool.Call.
	Call *tool.Call
}

// ToolHandler executes a tool call. The innermost handler runs the tool; each
// ToolWrapper contributes one layer around it.
type ToolHandler func(ctx context.Context, call *ToolCall) (tool.Result, error)
