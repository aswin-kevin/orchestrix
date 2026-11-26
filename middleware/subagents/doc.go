// Package subagents lets an agent delegate work to subagents through the
// task tool. Each task call starts a separate agent run that shares the
// parent's backend, streams its events into the parent's stream, and returns
// its final answer as the tool result. Its non-private state is merged back
// into the parent's when it finishes.
//
// A subagent runs in one of two modes:
//   - Isolated (every configured subagent, and the built-in general-purpose
//     one unless disabled): the run sees only the task description, under
//     its own system prompt.
//   - Fork (subagent_type Fork, unless Config.DisableFork): the run is the
//     parent's own agent continuing the parent's conversation, with the task
//     handed over as the result of its task call (ForkPrefix). System
//     prompt, tools and history prefix match the parent's, so the provider's
//     prompt cache is reused. A fork cannot delegate further, never
//     checkpoints and never syncs the workspace.
//
// Interrupts: with a parent Approver, subagents inherit it and ask it
// synchronously. Without one, a subagent that needs a decision ends
// interrupted; the parent keeps it (Suspended) and pauses the task call with
// one request that lists the subagent's pending actions and accepts approve
// and/or reject, whichever every one of those actions accepts. That
// request's Reason quotes each action's input cut short; SuspendedFor gives
// the host the subagent's full requests. Resuming the
// parent with that decision applies it to every pending action of the
// subagent and continues the subagent, which may pause again. Per-action
// decisions (edit, respond) on a subagent's requests are not available
// through the parent: a subagent whose actions accept neither approve nor
// reject is stopped, its state discarded, and the task call fails. A
// suspended subagent is dropped once its task call no longer awaits a
// decision, for instance when a new Run abandons the parent's interrupt. The
// subagent's own KindInterrupt events are not forwarded.
//
// Permissions: the task tool is read-only and allows itself, since starting
// a subagent changes nothing by itself and every tool the subagent calls is
// checked on its own. It is therefore allowed in plan and dontAsk modes too;
// a deny rule on "task" still refuses it. An isolated subagent starts in
// the stricter (permission.Stricter) of the parent's current mode (the one
// its run set with agent.SetPermissionMode, else its Config.PermissionMode)
// and its own configured mode (Subagent.PermissionMode, else the parent's
// Config.PermissionMode), so delegating never widens what may run without
// approval: a planning parent starts its subagents (and theirs) in plan
// mode, and a dontAsk parent cannot hand work to a bypassPermissions
// subagent. A fork continues the parent's State (State.Fork copies
// the mode agent.SetPermissionMode stored), so it runs in the parent's
// current mode.
//
// Skills: an isolated subagent is offered the skills its parent's run is
// offered (skills.ScopeKey), narrowed to Subagent.Skills when set, with
// Subagent.PinnedSkills injected into its prompt when set; its own subagents
// inherit that scope in turn. A fork continues the parent's State, so it keeps
// the parent's scope and loaded skills.
//
// Token usage: a subagent's model calls are reported as forwarded KindUsage
// events (with ParentToolID set) and are not included in the parent's
// Final.Usage.
package subagents
