package deferred

// DefaultPrompt is the system prompt segment that explains deferred tools.
// The middleware appends the names of the tools whose schemas are withheld.
const DefaultPrompt = `Some tools are available but their schemas are not loaded, so you cannot call them directly yet.

- Use search_tools to find one by keywords, or with "select:name1,name2" for exact names. A tool it finds can be called directly from your next turn on.
- call_tool calls a tool by exact name with an input object; prefer a direct call once the schema is loaded.
- Permission rules apply to every tool, whichever way it is called.`
