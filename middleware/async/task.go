package async

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/permission"
)

// episodeState is where a task entry is between its episodes. Every change
// is a compare-and-swap under the entry's mutex, so two callers that race to
// start the next episode (or to cancel a waiting one) cannot both win.
type episodeState int

const (
	// episodeStarting: the Runner is starting the first episode.
	episodeStarting episodeState = iota
	// episodeRunning: the current Handle's episode has not stopped.
	episodeRunning
	// episodeStopped: the current episode stopped (ended or waiting).
	episodeStopped
	// episodeClaimed: a follow-up or a decision holds the task while it
	// starts the next episode.
	episodeClaimed
	// episodeCancelled: the task waited and was cancelled; no episode
	// follows.
	episodeCancelled
)

// errStillRunning is the error of a follow-up for a task whose episode has
// not stopped, or whose next episode another caller is starting.
var errStillRunning = errors.New("async: task is still running; send it a message instead")

// errCancelledWaiting is the error of a follow-up for a task cancelled while
// it waited for a decision: its pending actions are never run, and the task
// does not go on.
var errCancelledWaiting = errors.New("async: the task was cancelled while it waited for a decision; start a new task instead")

// task is what the job registry holds for one background task, across its
// episodes. It is registered before the Runner starts the work, because the
// Runner's Start needs the task's id, which the registry hands out only with
// the registered job; attach then hands it the Runner's Handle, or fail ends
// it when the start failed. A follow-up (continueWith) swaps in the Handle of
// the next episode and re-arms done; jobs.Manager.Restart sees the same
// *task run again.
//
// Each episode's outcome is published before its done closes, so a job seen
// as ended always has one. done closes when the Handle's episode stops, also
// when the task now waits for a decision.
type task struct {
	// base is the start_task call's context (or for an adopted task, the
	// context that adopted it: an update_task call's, a run's start or a
	// host's Resolve), without its cancellation. An episode a host's
	// decision starts runs from it, with that context's values (a tool
	// call id, run id and trace span, when it has them); a follow-up runs
	// from the update_task call's context instead. Either way the runner
	// keeps the episode nested (agent.Input.Depth).
	base context.Context

	mu      sync.Mutex
	done    chan struct{}
	state   episodeState
	h       Handle          // nil until attached
	stopCtx context.Context // set when Stop came before attach
	out     Outcome
	ended   time.Time
	// meta is the identity of the task's record (ID, ThreadID,
	// TaskThreadID, SubagentType, Description, Started); settled, when set,
	// is told whenever an episode stops (see bind). epCtx is the context
	// the current episode was started from, without its cancellation.
	meta    TaskRecord
	settled settledFunc
	epCtx   context.Context
	// parentMode is the parent run's permission mode as last seen: at
	// start_task, at a follow-up and at each turn of a run of the parent's
	// thread. A host's decision passes it on (see Middleware.Resolve).
	parentMode permission.Mode
}

// settledFunc is told that an episode of t stopped with out (ended is when),
// on ctx, the context the episode was started from.
type settledFunc func(ctx context.Context, t *task, out Outcome, ended time.Time)

func newTask(base context.Context) *task {
	base = context.WithoutCancel(base)
	return &task{base: base, epCtx: base, done: make(chan struct{})}
}

// bind gives t the identity of its record and the function told when an
// episode stops; called before attach.
func (t *task) bind(meta TaskRecord, settled settledFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.meta, t.settled = meta, settled
}

// setParentMode records the parent run's current permission mode.
func (t *task) setParentMode(mode permission.Mode) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.parentMode = mode
}

// lastParentMode returns the parent run's permission mode as last seen; ""
// when never seen.
func (t *task) lastParentMode() permission.Mode {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.parentMode
}

// identity returns the identity of the task's record (see bind).
func (t *task) identity() TaskRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.meta
}

// Done implements jobs.Handle: the current episode's channel.
func (t *task) Done() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// handle returns the current episode's Handle; nil before attach.
func (t *task) handle() Handle {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.h
}

// Stop implements jobs.Handle: it stops the attached Handle with ctx, or the
// one attached later under ctx detached from its cancellation (it keeps its
// values) and bounded by DefaultStopWait.
func (t *task) Stop(ctx context.Context) error {
	t.mu.Lock()
	h := t.h
	if h == nil && t.stopCtx == nil {
		t.stopCtx = context.WithoutCancel(ctx)
	}
	t.mu.Unlock()
	if h == nil {
		return nil
	}
	return h.Stop(ctx)
}

// attach hands t the started Handle and follows it until its episode stops.
// A Handle whose episode already stopped is followed at once, so the task is
// ended when attach returns.
func (t *task) attach(h Handle) {
	t.mu.Lock()
	t.h, t.state = h, episodeRunning
	stopCtx, epCtx := t.stopCtx, t.epCtx
	done := t.done
	t.mu.Unlock()
	if stopCtx != nil {
		ctx, cancel := context.WithTimeout(stopCtx, DefaultStopWait)
		_ = h.Stop(ctx) // the outcome reports what the stop achieved
		cancel()
	}
	if isClosed(h.Done()) {
		t.follow(h, done, epCtx)
		return
	}
	go t.follow(h, done, epCtx)
}

// follow waits for h's episode to stop, publishes its outcome on done and
// then tells the settled function, on ctx, the episode's context.
func (t *task) follow(h Handle, done chan struct{}, ctx context.Context) {
	<-h.Done()
	out, ok := h.Outcome()
	if !ok {
		out = Outcome{Status: StatusFailed, Error: "the runner reported no outcome"}
	}
	out, ended := t.finish(out, done)
	t.mu.Lock()
	settled := t.settled
	t.mu.Unlock()
	if settled != nil {
		settled(ctx, t, out, ended)
	}
}

// settledCancelled tells the settled function that the waiting task was
// cancelled (see cancelWaiting), on the context of its last episode.
func (t *task) settledCancelled() {
	t.mu.Lock()
	settled, ctx, out, ended := t.settled, t.epCtx, t.out, t.ended
	t.mu.Unlock()
	if settled != nil {
		settled(ctx, t, out, ended)
	}
}

// fail ends a task whose Runner could not start it.
func (t *task) fail(err error) {
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	_, _ = t.finish(Outcome{Status: StatusFailed, Error: err.Error()}, done)
}

// finish publishes out, normalized, as the outcome of the episode of done
// and then closes done: a status other than waiting or terminal (lost
// included: only the middleware decides a task is lost), or waiting without
// a pending action, is failed, and Pending is kept only for a waiting task.
// It returns the published outcome and its time.
func (t *task) finish(out Outcome, done chan struct{}) (Outcome, time.Time) {
	switch out.Status {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusTimedOut:
		out.Pending = nil
	case StatusWaiting:
		if out.Pending == nil || len(out.Pending.Requests) == 0 {
			out.Status, out.Pending = StatusFailed, nil
			out.Error = joinError("the runner reported a waiting task with no pending action", out.Error)
		}
	default:
		out.Error = joinError(fmt.Sprintf("the runner reported status %q", out.Status), out.Error)
		out.Status, out.Pending = StatusFailed, nil
	}
	t.mu.Lock()
	ended := time.Now()
	t.out, t.ended = out, ended
	if t.state != episodeClaimed {
		t.state = episodeStopped
	}
	t.mu.Unlock()
	close(done)
	return out, ended
}

// continueWith starts the next episode of the ended task with message
// through its Handle's Continuer, on ctx's values, and swaps the new Handle
// in; caller is the context of the call that asked for it (see advance). It
// fails with ErrWaiting for a waiting task, with errCancelledWaiting for one
// cancelled while it waited, with errStillRunning for one whose episode
// runs or whose next episode another caller is starting, and with
// ErrFinished when the Handle is no Continuer. jobs.Manager.Restart calls it
// as its start function.
func (t *task) continueWith(ctx, caller context.Context, message string) error {
	return t.advance(caller, func() (func() (Handle, error), error) {
		switch {
		case t.state == episodeCancelled:
			return nil, errCancelledWaiting
		case t.state != episodeStopped:
			return nil, errStillRunning
		case t.out.Status == StatusWaiting:
			return nil, ErrWaiting
		}
		c, ok := t.h.(Continuer)
		if !ok {
			return nil, ErrFinished
		}
		return func() (Handle, error) { return c.Continue(ctx, message) }, nil
	})
}

// errNoResolver is the error of a decision for a waiting task whose Handle
// is no Resolver.
var errNoResolver = errors.New("async: the runner cannot resume a waiting task")

// resolveWith answers the waiting task's pending actions with decisions
// through its Handle's Resolver, starting the next episode on ctx's values,
// and swaps the new Handle in; caller is the context of the call that asked
// for it (see advance). It fails with ErrNotWaiting unless the task waits
// and no other caller is starting its next episode, and with errNoResolver
// when the Handle is no Resolver. jobs.Manager.Restart calls it as its start
// function.
func (t *task) resolveWith(ctx, caller context.Context, decisions []agent.Decision) error {
	return t.advance(caller, func() (func() (Handle, error), error) {
		if t.state != episodeStopped || t.out.Status != StatusWaiting {
			return nil, ErrNotWaiting
		}
		r, ok := t.h.(Resolver)
		if !ok {
			return nil, errNoResolver
		}
		return func() (Handle, error) { return r.Resolve(ctx, decisions) }, nil
	})
}

// advance starts the task's next episode. Only one caller wins: claim runs
// under the task's mutex and either refuses or returns the start function,
// and the task is then claimed until start returns, so a racing caller (a
// follow-up, a decision or a cancel) fails. On success the new Handle is
// swapped in and followed; its settled call gets caller without its
// cancellation.
func (t *task) advance(caller context.Context, claim func() (func() (Handle, error), error)) error {
	t.mu.Lock()
	start, err := claim()
	if err != nil {
		t.mu.Unlock()
		return err
	}
	t.state = episodeClaimed
	t.mu.Unlock()

	h, err := start()
	if err == nil && h == nil {
		err = errNoHandle
	}
	if err != nil {
		t.mu.Lock()
		t.state = episodeStopped
		t.mu.Unlock()
		return err
	}
	epCtx := context.WithoutCancel(caller)
	t.mu.Lock()
	done := make(chan struct{})
	t.h, t.done, t.state, t.epCtx = h, done, episodeRunning, epCtx
	t.out, t.ended = Outcome{}, time.Time{}
	t.mu.Unlock()
	go t.follow(h, done, epCtx)
	return nil
}

// waiting reports whether the task's episode stopped waiting for a
// decision that nobody gave or is giving yet.
func (t *task) waiting() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state == episodeStopped && t.out.Status == StatusWaiting
}

// cancelWaiting ends a waiting task: it claims it (so no follow-up starts
// an episode of it), turns its outcome into StatusCancelled and reports
// whether it was waiting. The caller then stops its Handle.
func (t *task) cancelWaiting() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != episodeStopped || t.out.Status != StatusWaiting {
		return false
	}
	t.state = episodeCancelled
	t.out.Status, t.out.Pending, t.ended = StatusCancelled, nil, time.Now()
	return true
}

// joinError is msg, followed by the runner's own error when there is one.
func joinError(msg, runnerErr string) string {
	if runnerErr != "" {
		return msg + ": " + runnerErr
	}
	return msg
}

// outcome returns how the current episode stopped and when; ok is false
// while it runs.
func (t *task) outcome() (out Outcome, ended time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !isClosed(t.done) {
		return Outcome{}, time.Time{}, false
	}
	return t.out, t.ended, true
}

// progress returns the attached Handle's progress, cut to its last
// DefaultProgressBytes bytes whatever the Runner reports; "" before attach.
func (t *task) progress() string {
	h := t.handle()
	if h == nil {
		return ""
	}
	return tail(h.Progress(), DefaultProgressBytes)
}

// send delivers msg to the attached Handle with ctx; ErrFinished once the
// episode stopped.
func (t *task) send(ctx context.Context, msg string) error {
	h := t.handle()
	if _, _, ok := t.outcome(); ok || h == nil {
		return ErrFinished
	}
	return h.Send(ctx, msg)
}

// isClosed reports whether ch is closed, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
