package planmode

// DefaultPrompt is the system prompt segment added while plan mode is
// active. It is neutral guidance only; the host supplies any persona.
const DefaultPrompt = `Plan mode is active. Only read-only tools work: you cannot edit files or run commands that change anything.

Investigate the task first, then call exit_plan_mode with a concrete plan (steps, files to change, how to verify). Do not try to edit files while planning; the user must approve the plan before you can implement it.`
