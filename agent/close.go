package agent

import (
	"context"
	"errors"
	"time"
)

// Closer is implemented by a middleware, backend or checkpointer that holds
// resources (connections, processes, background jobs) to release when the
// agent shuts down; see Agent.Close. ctx bounds how long Close may wait for
// them to end.
type Closer interface {
	Close(ctx context.Context) error
}

// ErrClosed is wrapped by the Final.Err of a run started after Agent.Close.
var ErrClosed = errors.New("agent: closed")

// Close releases what the agent holds, in this order: every middleware of
// Config.Middleware that implements Closer, in reverse stack order (those a
// MiddlewareExcluder left out of the run's stack included), then
// Config.Backend and Config.Checkpointer when they implement Closer, then the
// Config.OnClose functions in reverse order. Every one is called even when an
// earlier one fails; the errors are joined into the result and each is
// logged at Warn. A backend built by Config.BackendFactory is not closed:
// release those with an OnClose function.
//
// Close is idempotent and safe for concurrent use: the first call does the
// work, and every call returns its result once it is done.
//
// Runs already started when Close is called are not waited for or stopped:
// they keep going, and may fail once the resources they use are closed.
// Cancel their contexts first for a clean stop. A run started afterwards
// (Run, Resume, ResumeWith or a Thread's) does not start: its stream is a
// single KindFinal with ReasonInternalError whose Err wraps ErrClosed.
//
// Close only the top-level agent. Agents built from its Config (subagents,
// forks) share its middleware, backend, checkpointer and OnClose functions,
// so closing one of them releases the parent's resources; the library never
// closes them.
func (a *Agent) Close(ctx context.Context) error {
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		a.closeErr = a.closeResources(ctx)
	})
	return a.closeErr
}

// closeResources closes everything Close lists, in its order, and joins the
// errors.
func (a *Agent) closeResources(ctx context.Context) error {
	var errs []error
	report := func(kind, name string, err error) {
		if err == nil {
			return
		}
		a.logger.Warn("agent: close failed", "agent", a.cfg.Name, kind, name, "err", err)
		errs = append(errs, err)
	}
	mws := a.cfg.Middleware
	for i := len(mws) - 1; i >= 0; i-- {
		if c, ok := mws[i].(Closer); ok {
			report("middleware", mws[i].Name(), c.Close(ctx))
		}
	}
	if c, ok := a.cfg.Backend.(Closer); ok {
		report("resource", "backend", c.Close(ctx))
	}
	if c, ok := a.cfg.Checkpointer.(Closer); ok {
		report("resource", "checkpointer", c.Close(ctx))
	}
	for i := len(a.cfg.OnClose) - 1; i >= 0; i-- {
		if fn := a.cfg.OnClose[i]; fn != nil {
			report("resource", "on_close", fn(ctx))
		}
	}
	return errors.Join(errs...)
}

// closedEvent is the lone KindFinal of a run started after Close.
func (a *Agent) closedEvent(in Input) Event {
	final := &Final{Reason: ReasonInternalError, Err: ErrClosed, State: in.State, Version: in.Version}
	return Event{Kind: KindFinal, Agent: a.name(in), Time: time.Now(), Final: final}
}
