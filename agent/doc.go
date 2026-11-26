// Package agent runs the model/tool loop and defines the contracts that
// extend it.
//
// Compatibility: EventKind and TerminalReason may gain values in minor
// releases. Hosts ignore event kinds they do not know and treat unknown
// terminal reasons as failures. Interfaces in this package never gain
// methods; new hooks arrive as new optional interfaces. Agent, Thread and
// the permission mode are concrete on purpose: they are the fail-closed
// safety boundary.
//
// # Middleware
//
// Every optional behaviour of a run is a Middleware: a value with a Name and
// any of the hook interfaces below. The loop discovers hooks by type
// assertion, so a middleware implements only what it needs.
//
// Hooks run in this order: WrapRun once as a run starts, giving the run its
// context; ResolveOrphan for each unanswered tool call, before BeforeAgent, on
// runs that are not a Resume; BeforeAgent once at the start of a run; per
// turn, BeforeModel, the model call (through every ModelWrapper), then
// AfterModel; each tool call goes through every ToolWrapper; BeforeStop when a
// reply without tool calls would complete the run; AfterAgent once at the end;
// then the end functions WrapRun returned, before the final event. Before*
// hooks (BeforeAgent, BeforeModel) run in stack order; After* hooks
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
