package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/aswin-kevin/orchestrix/tool"
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
// that identify the run: thread_id, agent and run_id. It never returns nil: without
// a configured logger it drops every record. Middleware can use it to report
// errors it recovers from next to the loop's own. Log no message content or
// tool inputs: they are the caller's data.
func (i RunInfo) Logger() *slog.Logger {
	if i.Agent == nil {
		return discardLogger
	}
	return i.Agent.logger.With("thread_id", i.ThreadID, "agent", i.AgentName, "run_id", i.RunID)
}

// logPanic logs a recovered panic at level: the type of the panic value and
// the stack, never the value, which may carry the caller's data.
func logPanic(l *slog.Logger, level slog.Level, msg string, v any, stack []byte) {
	l.Log(context.Background(), level, msg, "panic_type", fmt.Sprintf("%T", v), "stack", string(stack))
}

// warnOnce reports a dynamic tool that Registry.Add refused, once per run
// for each tool name. The loop's snapshots run every turn and would
// otherwise repeat the record.
type warnOnce struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func (w *warnOnce) skipped(l *slog.Logger, t tool.Tool, err error) {
	name := ""
	if t != nil {
		name = t.Name()
	}
	w.mu.Lock()
	_, dup := w.seen[name]
	if w.seen == nil {
		w.seen = map[string]struct{}{}
	}
	w.seen[name] = struct{}{}
	w.mu.Unlock()
	if !dup {
		l.Warn("agent: dynamic tool skipped", "tool", name, "err", err)
	}
}
