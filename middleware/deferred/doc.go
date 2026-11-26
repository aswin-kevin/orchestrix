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
// Both kinds share one per-run unlock set (see Unlock and Unlocked);
// search_tools also records the names it unlocked in a second private key,
// read with Searched, so a middleware that relocks its own grants can leave
// those alone. Nothing here weakens a permission check: call_tool is a
// tool.Dispatcher, so the agent loop runs its target through the same
// permission rules, mode, approvals, hitl and hooks as a direct call, and the
// Defer and Gate wrappers forward every permission-relevant interface of the
// tool they wrap.
package deferred
