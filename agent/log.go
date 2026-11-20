package agent

import (
	"log/slog"
)

// discardLogger is the logger of an agent whose Config.Logger is nil.
var discardLogger = slog.New(slog.DiscardHandler)

// loggerOrDiscard returns l, or a logger that drops every record when l is
// nil.
func loggerOrDiscard(l *slog.Logger) *slog.Logger {
	if l == nil {
		return discardLogger
	}
	return l
}

// Logger returns the agent's logger (Config.Logger) with the attributes
// that identify the run: thread_id and agent. It never returns nil: without
// a configured logger it drops every record. Middleware can use it to report
// errors it recovers from next to the loop's own. Log no message content or
// tool inputs: they are the caller's data.
func (i RunInfo) Logger() *slog.Logger {
	if i.Agent == nil {
		return discardLogger
	}
	// Task 22 adds run_id here.
	return i.Agent.logger.With("thread_id", i.ThreadID, "agent", i.AgentName)
}
