// Package agent runs the model/tool loop and defines the contracts that
// extend it.
//
// # Middleware
//
// Every optional behaviour of a run is a Middleware: a value with a Name and
// any of the hook interfaces below. The loop discovers hooks by type
// assertion, so a middleware implements only what it needs.
//
// Hooks run in this order: WrapRun once as a run starts, giving the run its
// context; ResolveOrphan for each unanswered tool call, before BeforeAgent, on
// runs that are not a Resume; BeforeAgent once at the start of a run; per turn,
// BeforeModel, the model call (through every ModelWrapper), then AfterModel;
// each tool call goes through every ToolWrapper; BeforeStop when a reply
// without tool calls would complete the run; AfterAgent once at the end;
// then the end functions WrapRun returned, before the final event.
// Before* hooks (BeforeAgent, BeforeModel) run in stack order; After* hooks
// (AfterModel, AfterAgent) and BeforeStop run in reverse stack order,
// mirroring how wrappers unwind.
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

	"github.com/aswin-kevin/orchestrix/backend"
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

// RunWrapper wraps a whole run. WrapRun is called once per run (Run, Resume
// and ResumeWith alike), in stack order, before any other hook of the run;
// info is the run's RunInfo, with Depth and ParentToolID set. A Thread asked
// to run while busy starts no run and calls no wrapper.
//
// The returned context becomes the run's context for everything that
// follows: hooks, model and tool wrappers, tool calls and the runs nested in
// them. It must be derived from ctx and keep its cancellation; a context
// that lacks the run's RunInfo
// is a programming error, and the run panics naming the middleware.
//
// The end function, which may be nil, is called exactly once per run, in
// reverse stack order, with the run's Final: after the AfterAgent hooks and
// the end-of-run checkpoint, before KindFinal is delivered, and also when
// the consumer stopped reading the run's events (when AfterAgent is
// skipped). A panic in it is recovered and discarded, so it cannot break the
// run. It must not emit events.
type RunWrapper interface {
	WrapRun(ctx context.Context, info RunInfo) (context.Context, func(final *Final))
}

// Orphan is a tool_use in State that has no tool_result.
type Orphan struct {
	// ID is the tool_use id.
	ID string
	// Name is the tool the model called, as in the tool_use.
	Name string
	// Input is the raw input, as in the tool_use.
	Input json.RawMessage
	// Saved is a copy of the result an abandoned interrupted turn recorded
	// for this call; nil when it has none (crash, token limit, or never
	// started). Its Content slice is a copy; nested block data may be shared
	// with State, so do not modify it.
	Saved *tool.Result
}

// OrphanResolver supplies the result of a tool call whose result never
// reached State: the process crashed mid-turn, a token limit ended the run
// after a reply that called tools, or a new run abandoned an interrupted turn.
//
// ResolveOrphan runs for every such call in State, in history order, at the
// start of each run that is not a Resume (Run, Send, Thread.Prompt and
// Thread.Send), after the lease is taken and before the new input is
// appended, so before BeforeAgent. Resolvers are asked in stack order and the
// first to return ok=true wins; ok=false declines and the next is asked. When
// none answers, Orphan.Saved is used if set, else an error result saying the
// call was interrupted. Every orphan thus ends up with a result in State. The
// answers are stored at the run's next checkpoint; a crash before that
// checkpoint asks again, so a resolver should be idempotent.
//
// Results are normally appended, since the orphan is the last message. An
// insertion before older messages shifts the indices after it, which
// elisions follow but a summarize Boundary does not.
//
// Orchestrix never runs an orphaned call again. A host that knows the call
// never ran returns an error result saying so, and the model re-issues it
// through the normal pipeline.
//
// The result is truncated to Config.MaxToolOutputChars like any tool result;
// its Extra messages are ignored. An error ends the run with
// ReasonMiddlewareError, or with ReasonHookStopped when it wraps ErrStopRun,
// and so does a panic, which is recovered. ctx is the run's context, so
// cancellation reaches the resolver.
type OrphanResolver interface {
	ResolveOrphan(ctx context.Context, s *state.State, o Orphan) (res tool.Result, ok bool, err error)
}

// BeforeAgent runs once before the first turn. A returned error ends the run
// with ReasonMiddlewareError, or with ReasonHookStopped when it wraps
// ErrStopRun; the same holds for BeforeModel, AfterModel and BeforeStop.
type BeforeAgent interface {
	BeforeAgent(ctx context.Context, s *state.State) error
}

// AfterAgent runs once after the run has finished, whatever the reason,
// unless the consumer stopped reading the run's events first. Its context
// carries the run's values but is never cancelled, so it can still clean up
// after a cancelled run.
type AfterAgent interface {
	AfterAgent(ctx context.Context, s *state.State) error
}

// BeforeModel runs before each model call, before the request is built from
// State, so changes it makes to State are part of the request.
type BeforeModel interface {
	BeforeModel(ctx context.Context, s *state.State) error
}

// AfterModel runs after each successful model call with the response, before
// any tool calls it contains are executed.
type AfterModel interface {
	AfterModel(ctx context.Context, s *state.State, resp *llm.Response) error
}

// BeforeStop runs when the model ended a turn without tool calls and the run
// is about to complete. It is not consulted during settlement (the run ends
// at its limit whatever the hook says), nor while a reply cut off at the
// output limit is being resumed, nor after MaxStopContinuations
// continuations in the run. With Config.OutputSchema, the final answer is
// checked before these hooks run: an invalid one is sent back for correction
// without consulting them and without using their continuations, and a
// valid one that a hook continues is checked again on the next reply.
//
// A non-empty prompt is appended to State as a user message marked
// InjectedMetaKey (ContinuationMetaValue) and the run takes another turn; an empty one lets the run complete. Hooks run in reverse
// stack order, like AfterModel, and the first non-empty prompt wins: the
// hooks after it are not consulted. A returned error ends the run like an
// AfterModel error.
type BeforeStop interface {
	BeforeStop(ctx context.Context, s *state.State, resp *llm.Response) (prompt string, err error)
}

// MaxStopContinuations is how many times per run BeforeStop hooks may keep
// the run going; after that they are not consulted and the run completes.
// The slots are shared by every BeforeStop middleware of the stack: a hook
// consulted later in reverse stack order (a hooks Stop quality gate, say)
// does not run at all on a reply that ends the run after the slots were
// spent by the hooks consulted before it.
const MaxStopContinuations = 3

// ModelWrapper wraps the model call. See the package documentation for the
// wrapping semantics. The ModelRequest is shared by every layer, and inner
// wrappers edit it in place (appending system segments, filtering tools): a
// wrapper that calls next more than once must restore the request it passed
// first, or each retry carries the inner edits again.
type ModelWrapper interface {
	WrapModel(ctx context.Context, req *ModelRequest, next ModelHandler) (*llm.Response, error)
}

// ToolWrapper wraps a single tool execution. See the package documentation
// for the wrapping semantics. Returning an error wrapping ErrStopRun ends the
// run with ReasonHookStopped once the calls already running are done.
type ToolWrapper interface {
	WrapTool(ctx context.Context, call *ToolCall, next ToolHandler) (tool.Result, error)
}

// ToolProvider lets a middleware contribute tools to the run, such as a
// middleware that owns a task-management tool.
type ToolProvider interface {
	Tools() []tool.Tool
}

// DynamicToolProvider lets a middleware contribute tools whose set may change
// while the agent lives, such as the tools of a remote server. DynamicTools
// is called once per model turn (and by Agent.Tools and Agent.Tool); it must
// be fast (no I/O) and safe for concurrent use. A dynamic tool that is nil,
// whose name is invalid or whose name is already taken (by Config.Tools, a
// ToolProvider, or an earlier dynamic tool in stack order) is skipped. A
// call is resolved against the tools of the turn that made it; a call
// resumed later is resolved against the set current then, so a tool that
// left the set fails as an unknown tool.
type DynamicToolProvider interface {
	DynamicTools() []tool.Tool
}

// SubagentToolName is the name of the tool that starts a subagent. Middleware
// that must recognize subagent launches (hooks, the subagents package) share
// this one definition.
const SubagentToolName = "task"

// InjectedMetaKey marks, in llm.Message.Meta, a user message that a
// middleware added to the conversation on its own, such as a background
// job's notification, a hook's context or a reminder, rather than input from
// the user. Its value names what added it. Such a message does not start a
// new user turn: middleware that reasons about turns (summarize, compress,
// rubric) skips it.
//
// The agent core marks its own continuations this way, with the value
// ContinuationMetaValue: a BeforeStop prompt, the resume after a reply cut
// off at max tokens, a token-budget nudge and a settlement prompt. Provider
// requests never carry Meta.
const InjectedMetaKey = "orchestrix.injected"

// ContinuationMetaValue is the InjectedMetaKey value of a user message the
// agent core appended to keep the run going (see InjectedMetaKey).
const ContinuationMetaValue = "stop"

// ModelRequest is everything a model call needs, as seen by ModelWrappers.
type ModelRequest struct {
	// Provider is the provider the innermost handler calls.
	Provider llm.Provider
	// Request is a view: editing its Messages never touches State.
	Request llm.Request
	// State is the run's conversation state.
	State *state.State
	// Profile is the model profile in effect (Config.Profile or the one
	// looked up for Config.Model).
	Profile llm.Profile
	// Turn is the zero-based index of the current model turn.
	Turn int
	// Backend is the backend resolved for this run (Input.Backend,
	// BackendFactory(threadID) or Config.Backend). It may be nil.
	Backend backend.Backend
	// ThreadID is the run's Input.ThreadID; empty when it was started
	// without one.
	ThreadID string
}

// ModelHandler performs a model call. The innermost handler talks to the
// provider; each ModelWrapper contributes one layer around it.
type ModelHandler func(ctx context.Context, req *ModelRequest) (*llm.Response, error)

// ToolCall describes one tool execution as seen by ToolWrappers. Changing
// Input before calling next changes what the tool receives.
type ToolCall struct {
	// ID is the tool_use id of the call.
	ID string
	// Name is the tool that runs: for a dispatched call, the target.
	Name string
	// Via is the name of the dispatching tool (tool.Dispatcher) when the
	// model called Name through it; empty when the model called Name
	// directly.
	Via string
	// Input is the call's raw JSON arguments; a wrapper may replace it
	// before calling next.
	Input json.RawMessage
	// Tool is the tool that executes the call.
	Tool tool.Tool
	// State is the run's conversation state.
	State *state.State
	// Call is the per-invocation environment passed to Tool.Call.
	Call *tool.Call
	// Approved reports that a human approved this call: the Approver allowed
	// it, or it runs on a Resume decision. False for a call the rules or the
	// mode allowed without asking anyone.
	Approved bool
}

// ToolHandler executes a tool call. The innermost handler runs the tool; each
// ToolWrapper contributes one layer around it.
type ToolHandler func(ctx context.Context, call *ToolCall) (tool.Result, error)
