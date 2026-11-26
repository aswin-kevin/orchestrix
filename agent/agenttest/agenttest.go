package agenttest

import (
	"context"
	"slices"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/internal/runctx"
)

// Recorder collects the events sent with agent.Emit on a context built by
// Context. It is safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	events []agent.Event
}

// Events returns a copy of the events recorded so far, in the order they
// were sent.
func (r *Recorder) Events() []agent.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// emit records ev; like agent.Emit it refuses KindFinal.
func (r *Recorder) emit(ev agent.Event) bool {
	if ev.Kind == agent.KindFinal {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return true
}

// Context returns parent carrying info as the run it belongs to, for unit
// tests of middleware hooks, wrappers and tools: agent.Info returns info,
// agent.ThreadID returns info.ThreadID, and agent.Emit records its events in
// the returned Recorder, as they were given (nothing is filled in). info.Agent
// may be nil; RunInfo.Tools is then empty.
func Context(parent context.Context, info agent.RunInfo) (context.Context, *Recorder) {
	rec := &Recorder{}
	ctx := context.WithValue(parent, runctx.InfoKey{}, &info)
	return context.WithValue(ctx, runctx.EmitKey{}, rec.emit), rec
}
