// Package summarize keeps long conversations inside the model's context
// window without losing anything from State.
//
// The history in State always stays complete; only the request view changes.
// Once older messages are compacted, a Boundary records how many leading
// messages a summary stands for, and every request is built as one user
// message holding the summary followed by the messages after the boundary.
// The summarized originals are also saved to the backend, one file per
// compaction under HistoryDir/<thread>, so the model can read them back; runs
// without a ThreadID skip this.
//
// Elisions (package agent/elide) apply to the messages after the boundary as
// they do in the loop's view (elide.View), and the size estimate that
// triggers compaction measures that elided view. Cuts and summaries stay in
// State space: a summary always covers the original messages, elided or not.
//
// Every request is micro-compacted as well: old, large tool results are
// replaced by a short notice and old, large tool inputs are shortened. The
// current turn is never touched: results the model has not seen yet are
// kept, and so are the tool inputs of the tool loop answering the last user
// message, which may carry signed thinking that providers validate. The
// model can ask for a compaction itself with the compact_conversation tool.
//
// Compaction also runs by itself: before a model call when the estimated
// request size passes TriggerFraction of the context window, and, once per
// call, when the provider rejects a request as too long (the retry keeps a
// smaller tail). After MaxFailures failed or fruitless compactions in a row
// the automatic trigger stops, which Boundary.LastError reports; the next
// successful compaction or the start of a new run closes the breaker again.
//
// The cut between summary and kept messages never separates a tool_result
// from its tool_use, so the view is always a valid conversation.
//
// The summary itself comes from a Summarizer: by default a ModelSummarizer,
// one model call with Config.SummaryPrompt as the system prompt, made with
// Config.Provider and Config.Model or else the request's own. Set
// Config.Summarizer to produce summaries another way.
//
// This middleware must be the first ModelWrapper in the stack: it rebuilds
// the request messages from State, which would discard any rewrite made by a
// wrapper that ran before it.
package summarize
