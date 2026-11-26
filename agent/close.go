package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

// Closer is implemented by a middleware, backend or checkpointer that holds
// resources (connections, processes, background jobs) to release when the
// agent shuts down; see Agent.Close. ctx bounds how long Close may wait for
// them to end. It is backend.Closer.
type Closer = backend.Closer

// Drainer is implemented by a middleware that runs work of its own beside
// the agent's runs, such as background agents, which use the resources of
// the stack. Agent.Close drains every Drainer before it closes anything, so
// that work stops while the resources it uses are still open. A middleware
// that runs nested agents in the background should implement it. ctx bounds
// how long Drain may wait for the work to end.
type Drainer interface {
	Drain(ctx context.Context) error
}

// ErrClosed is wrapped by the Final.Err of a run started after Agent.Close.
var ErrClosed = errors.New("agent: closed")

// Close releases what the agent holds, in two phases. First it drains: every
// middleware of Config.Middleware that implements Drainer, in stack order,
// stops its own work (background agents, say), which may still use the
// resources closed next. Then it closes: every middleware of
// Config.Middleware that implements Closer, in reverse stack order, then
// Config.Backend when it, or a backend it wraps, implements Closer or
// io.Closer (see backend.AsCloser), and Config.Checkpointer when it
// implements Closer (or, failing that, io.Closer); an io.Closer's Close takes
// no ctx and so is not bounded by it. Last come the Config.OnClose
// functions, in reverse order. Both phases include the middleware a
// MiddlewareExcluder left out of the run's stack.
// Every one is called even when an earlier one fails or panics: the errors
// are joined into the result and each is logged at Warn; a panic is
// recovered, joined as a *PanicError and logged at Error by type and stack
// only. A backend built by Config.BackendFactory is not closed: release
// those with an OnClose function.
//
// Close is idempotent and safe for concurrent use: the first call does the
// work, and every call returns its result once it is done. That result is
// final: a later call with a longer deadline does not retry. A host whose
// first Close ran out of time while background work was still ending can
// wait longer through the middleware itself (tasks or async Shutdown, for
// instance).
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

// closeResources drains and closes everything Close lists, in its order,
// and joins the errors.
func (a *Agent) closeResources(ctx context.Context) error {
	var errs []error
	call := func(kind, name string, fn func(context.Context) error) {
		if err := a.closeStep(ctx, kind, name, fn); err != nil {
			errs = append(errs, err)
		}
	}
	mws := a.cfg.Middleware
	for _, mw := range mws {
		if d, ok := mw.(Drainer); ok {
			call("middleware", mw.Name(), d.Drain)
		}
	}
	for i := len(mws) - 1; i >= 0; i-- {
		if c, ok := mws[i].(Closer); ok {
			call("middleware", mws[i].Name(), c.Close)
		}
	}
	if c, ok := backend.AsCloser(a.cfg.Backend); ok {
		call("resource", "backend", c.Close)
	}
	if fn := resourceCloser(a.cfg.Checkpointer); fn != nil {
		call("resource", "checkpointer", fn)
	}
	for i := len(a.cfg.OnClose) - 1; i >= 0; i-- {
		if fn := a.cfg.OnClose[i]; fn != nil {
			call("resource", "on_close", fn)
		}
	}
	return errors.Join(errs...)
}

// resourceCloser returns how Close releases v: through Closer, or through
// io.Closer when v implements only that; nil when it implements neither.
func resourceCloser(v any) func(context.Context) error {
	switch c := v.(type) {
	case Closer:
		return c.Close
	case io.Closer:
		return func(context.Context) error { return c.Close() }
	}
	return nil
}

// closeStep runs one drain or close function of Close and logs its error,
// recovering a panic as a *PanicError logged by type and stack only.
func (a *Agent) closeStep(ctx context.Context, kind, name string, fn func(context.Context) error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			perr := &PanicError{Value: v, Stack: debug.Stack()}
			logPanic(a.logger.With("agent", a.cfg.Name, kind, name), slog.LevelError, "agent: close panicked", v, perr.Stack)
			err = perr
		}
	}()
	if err = fn(ctx); err != nil {
		a.logger.Warn("agent: close failed", "agent", a.cfg.Name, kind, name, "err", err)
	}
	return err
}

// closedEvent is the lone KindFinal of a run started after Close.
func (a *Agent) closedEvent(in Input) Event {
	final := &Final{Reason: ReasonInternalError, Err: ErrClosed, State: in.State, Version: in.Version}
	return Event{Kind: KindFinal, Agent: a.name(in), Time: time.Now(), Final: final}
}
