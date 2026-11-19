package askuser

// DefaultPrompt is the system prompt segment that tells the model when to
// use ask_user. It is neutral guidance only; the host supplies any persona.
const DefaultPrompt = `You can ask the user structured multiple-choice questions with the ask_user tool.

Use it when a decision is the user's to make and you cannot settle it by investigating: ambiguous requirements, a choice between approaches, a preference. Ask 1-4 related questions at once, each with 2-4 distinct options; the user may always answer in their own words. Do not use it to ask for permission to run a tool or for information you can find yourself.`
