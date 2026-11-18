package planning

// DefaultPrompt is the system prompt segment that explains write_todos. It is
// neutral guidance only; the host supplies any persona.
const DefaultPrompt = `You can track your plan with the write_todos tool.

- Use it for tasks that take several steps; skip it for simple one-step requests.
- Send the complete list each time. Keep exactly one item in_progress while you work.
- Mark an item completed as soon as it is done, instead of batching updates.`
