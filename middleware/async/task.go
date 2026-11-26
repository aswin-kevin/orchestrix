package async

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
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
	// episodeClaimed: a Continue holds the task while it starts the next
	// episode.
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
	// base is the context every later episode starts from: the start_task
	// call's (or for an adopted task, the call's that adopted it), without
	// its cancellation. It keeps the parent run's values, so an episode runs
	// nested (agent.RunInfo.Depth is the parent's plus one).
	base context.Context

	mu      sync.Mutex
	done    chan struct{}
	state   episodeState
	h       Handle          // nil until attached
	stopCtx context.Context // set when Stop came before attach
	out     Outcome
	ended   time.Time
}

func newTask(base context.Context) *task {
	return &task{base: context.WithoutCancel(base), done: make(chan struct{})}
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
	stopCtx := t.stopCtx
	done := t.done
	t.mu.Unlock()
	if stopCtx != nil {
		ctx, cancel := context.WithTimeout(stopCtx, DefaultStopWait)
		_ = h.Stop(ctx) // the outcome reports what the stop achieved
		cancel()
	}
	if isClosed(h.Done()) {
		t.follow(h, done)
		return
	}
	go t.follow(h, done)
}

// follow waits for h's episode to stop and publishes its outcome on done.
func (t *task) follow(h Handle, done chan struct{}) {
	<-h.Done()
	out, ok := h.Outcome()
	if !ok {
		out = Outcome{Status: StatusFailed, Error: "the runner reported no outcome"}
	}
	t.finish(out, done)
}

// fail ends a task whose Runner could not start it.
func (t *task) fail(err error) {
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	t.finish(Outcome{Status: StatusFailed, Error: err.Error()}, done)
}

// finish publishes out, normalized, as the outcome of the episode of done
// and then closes done: a status other than waiting or terminal (lost
// included: only the middleware decides a task is lost), or waiting without
// a pending action, is failed, and Pending is kept only for a waiting task.
func (t *task) finish(out Outcome, done chan struct{}) {
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
	t.out, t.ended = out, time.Now()
	if t.state != episodeClaimed {
		t.state = episodeStopped
	}
	t.mu.Unlock()
	close(done)
}

// continueWith starts the next episode of the ended task with message
// through its Handle's Continuer, on ctx's values (normally t.base's), and
// swaps the new Handle in. Only one caller wins: the task is claimed under
// its mutex first, and a caller that finds it running or claimed fails with
// errStillRunning. It fails with ErrWaiting for a waiting task, with
// errCancelledWaiting for one cancelled while it waited, and with
// ErrFinished when the Handle is no Continuer. jobs.Manager.Restart calls it
// as its start function.
func (t *task) continueWith(ctx context.Context, message string) error {
	t.mu.Lock()
	switch {
	case t.state == episodeCancelled:
		t.mu.Unlock()
		return errCancelledWaiting
	case t.state != episodeStopped:
		t.mu.Unlock()
		return errStillRunning
	case t.out.Status == StatusWaiting:
		t.mu.Unlock()
		return ErrWaiting
	}
	c, ok := t.h.(Continuer)
	if !ok {
		t.mu.Unlock()
		return ErrFinished
	}
	t.state = episodeClaimed
	t.mu.Unlock()

	h, err := c.Continue(ctx, message)
	if err == nil && h == nil {
		err = errNoHandle
	}
	if err != nil {
		t.mu.Lock()
		t.state = episodeStopped
		t.mu.Unlock()
		return err
	}
	t.mu.Lock()
	done := make(chan struct{})
	t.h, t.done, t.state = h, done, episodeRunning
	t.out, t.ended = Outcome{}, time.Time{}
	t.mu.Unlock()
	go t.follow(h, done)
	return nil
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
