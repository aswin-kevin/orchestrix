package async

// DefaultPrompt is the system prompt segment that explains background tasks.
// It is neutral guidance only; the host supplies any persona.
const DefaultPrompt = `You can run subagents in the background with start_task and keep working while they run.

- Use start_task for independent work that can proceed in parallel with yours; use the task tool instead when you need the answer before you can continue.
- A background subagent knows nothing of this conversation: put every detail it needs in the description, and say what it should return.
- You are notified with a message when a task finishes. Use check_task to see a task's progress, or with wait set to wait for it; use list_tasks to see every task and cancel_task to stop one you no longer need.
- Use update_task to send a running task new instructions or information; it reads them before its next step. Sent to a task that finished, it continues that task with everything it did so far, which is cheaper than starting a new one for a follow-up.
- A task may stop waiting for a decision, such as an approval to run a tool. Only the user can make it, not you: carry on with other work or tell the user what the task waits for. You can still cancel it.
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

// updateDescription is update_task's description; %d is MaxUpdateBytes.
const updateDescription = "Send a message to a background task, such as new instructions or information it needs. A running task reads it before its next step; a task that ended continues with it as a follow-up, with everything it did so far, and you are notified again when it finishes. At most %d bytes."

// stopReminder is the prompt of a run about to end while tasks of its
// thread still run; %s lists their ids.
const stopReminder = "Background tasks are still running: %s. Wait for them with check_task (wait: true) or cancel them before giving your final answer."
