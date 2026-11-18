package subagents

// DefaultPrompt is the system prompt segment that explains delegation with
// the task tool. It is neutral guidance only; the host supplies any persona.
const DefaultPrompt = `You can delegate work to subagents with the task tool.

- Delegate self-contained, multi-step work (research, broad searches, independent changes) whose intermediate steps you do not need to see.
- A subagent knows nothing of this conversation: put every detail, path and constraint it needs in the description, and say what it should return.
- Start independent tasks in the same turn so they run in parallel.
- Do simple lookups and single edits yourself; delegating them only adds overhead.
- The subagent's final answer comes back to you, not to the user; pass on what matters.`

// DefaultGeneralPurposePrompt is the system prompt of the built-in
// general-purpose subagent.
const DefaultGeneralPurposePrompt = `You are a subagent carrying out one task for another agent. Complete it with the tools available to you, then reply with a concise final answer that holds everything the requesting agent needs: findings, decisions, and the paths of any files you changed. Your final reply is the only thing the requesting agent sees.`

// generalPurposeDescription tells the parent model when to pick the built-in
// general-purpose subagent.
const generalPurposeDescription = "A general-purpose agent with the same tools as you, for research and multi-step tasks that are best done in a separate context."

// taskDescription is the task tool's description; the list of available
// subagents follows it.
const taskDescription = `Start a subagent that carries out a task in its own, separate context and returns its final answer. The subagent does not see this conversation, so the description must contain everything it needs. Several task calls in one turn run in parallel.

Available subagents:`

// outputFormatPrompt introduces the JSON schema a subagent with an
// OutputSchema must answer with; the schema follows it.
const outputFormatPrompt = "When you finish, reply with only a JSON object, with no other text, that is valid against this JSON schema:\n"

// invalidOutputPrompt asks a subagent to correct an answer that failed the
// OutputSchema; %v is the validation error.
const invalidOutputPrompt = "Your answer is not valid: %v. Reply with only the JSON object."

// noTextResult is the task result of a subagent that completed without text.
const noTextResult = "(the subagent returned no text)"
