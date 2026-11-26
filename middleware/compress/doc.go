// Package compress lets the model compress older parts of its own
// conversation.
//
// The engine decides when and what: once the estimated request size passes
// TriggerFraction of the context window, it proposes the oldest settled range
// of whole user turns (a Proposal) in a <compress_request> system segment.
// The model answers by calling compress_context with a summary of that
// range. The range then becomes an elision (elide.Add): the request
// view shows the summary in its place, while State keeps every original
// message. restore_context removes the elision, so the range shows verbatim
// again; it needs no archive. With a backend and a thread, the originals are
// also archived under ArchiveDir/<thread>.
//
// A proposal never covers the newest KeepTurns user turns, so the current
// turn (the run's input and the tool loop answering it) is never touched.
// Only user input starts a turn: a message marked agent.InjectedMetaKey,
// such as a stop continuation the agent core appended (rubric feedback, a
// reminder, a resume prompt) or a background notification, does not.
// It starts at or after summarize's boundary, never straddles an existing
// elision, and stays clear of any call left unanswered. A proposal the model
// ignores closes after DefaultProposalTurns model turns; after MaxIgnored
// ignored proposals the run stops proposing.
//
// The summary is text the model wrote, possibly from untrusted input, so any
// <compressed> or <compress_request> tag inside it is neutralized before it
// enters the view. The <compress_request> segment itself quotes short
// excerpts (excerptChars characters) of the messages that bound the range,
// which may be user or tool text: this is the one place where such text
// enters a system segment rather than a user or tool message. The excerpts
// are kept on one line, neutralized for the compress tags and quoted as Go
// strings, and serve only to locate the range; the design keeps the request
// next to the instructions it belongs to.
//
// Put this middleware after summarize: summarize rebuilds the request
// messages from State, and compress only appends system segments and never
// rewrites the messages, nor changes elisions while a request is in flight.
package compress
