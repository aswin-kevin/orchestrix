package async

// DefaultPrompt is the system prompt segment that explains background tasks.
// It is neutral guidance only; the host supplies any persona.
const DefaultPrompt = `You can run subagents in the background with start_task and keep working while they run.

- Use start_task for independent work that can proceed in parallel with yours; use the task tool instead when you need the answer before you can continue.
- A background subagent knows nothing of this conversation: put every detail it needs in the description, and say what it should return.
- You are notified with a message when a task finishes. Use check_task to see a task's progress, or with wait set to wait for it; use list_tasks to see every task and cancel_task to stop one you no longer need.
- Do not give your final answer while a task whose result you need is still running.`

// startDescription is start_task's description; the list of available
// subagents follows it.
const startDescription = `Start a subagent in the background and return at once with its task id. The subagent works in its own, separate context and does not see this conversation, so the description must contain everything it needs. You are notified when it finishes.

Available subagents:`

// checkDescription is check_task's description.
const checkDescription = "Show a background task's status with its recent output, or its result once it finished. With wait, wait until it finishes or timeout_seconds pass."

// cancelDescription is cancel_task's description.
const cancelDescription = "Cancel a running background task."

// listDescription is list_tasks' description.
const listDescription = "List this conversation's background tasks with their status."
