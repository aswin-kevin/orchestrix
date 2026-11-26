package async

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// task is what the job registry holds for one background task. It is
// registered before the Runner starts the work, because the Runner's Start
// needs the task's id, which the registry hands out only with the registered
// job; attach then hands it the Runner's Handle, or fail ends it when the
// start failed.
//
// Its outcome is published before done closes, so a job seen as ended always
// has one. done closes when the Handle's episode stops, also when the task
// now waits for a decision.
type task struct {
	done chan struct{}

	mu      sync.Mutex
	h       Handle          // nil until attached
	stopCtx context.Context // set when Stop came before attach
	out     Outcome
	ended   time.Time
}

func newTask() *task { return &task{done: make(chan struct{})} }

// Done implements jobs.Handle.
func (t *task) Done() <-chan struct{} { return t.done }

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
func (t *task) attach(h Handle) {
	t.mu.Lock()
	t.h = h
	stopCtx := t.stopCtx
	t.mu.Unlock()
	if stopCtx != nil {
		ctx, cancel := context.WithTimeout(stopCtx, DefaultStopWait)
		_ = h.Stop(ctx) // the outcome reports what the stop achieved
		cancel()
	}
	go func() {
		<-h.Done()
		out, ok := h.Outcome()
		if !ok {
			out = Outcome{Status: StatusFailed, Error: "the runner reported no outcome"}
		}
		t.finish(out)
	}()
}

// fail ends a task whose Runner could not start it.
func (t *task) fail(err error) {
	t.finish(Outcome{Status: StatusFailed, Error: err.Error()})
}

// finish publishes out, normalized, and then closes done: a status other
// than waiting or terminal (lost included: only the middleware decides a
// task is lost), or waiting without a pending action, is failed, and Pending
// is kept only for a waiting task.
func (t *task) finish(out Outcome) {
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
	t.mu.Unlock()
	close(t.done)
}

// joinError is msg, followed by the runner's own error when there is one.
func joinError(msg, runnerErr string) string {
	if runnerErr != "" {
		return msg + ": " + runnerErr
	}
	return msg
}

// outcome returns how the episode stopped and when; ok is false while it
// runs.
func (t *task) outcome() (out Outcome, ended time.Time, ok bool) {
	select {
	case <-t.done:
	default:
		return Outcome{}, time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out, t.ended, true
}

// progress returns the attached Handle's progress, cut to its last
// DefaultProgressBytes bytes whatever the Runner reports; "" before attach.
func (t *task) progress() string {
	t.mu.Lock()
	h := t.h
	t.mu.Unlock()
	if h == nil {
		return ""
	}
	return tail(h.Progress(), DefaultProgressBytes)
}

// send delivers msg to the attached Handle with ctx; ErrFinished once the
// episode stopped.
func (t *task) send(ctx context.Context, msg string) error {
	t.mu.Lock()
	h := t.h
	t.mu.Unlock()
	if _, _, ok := t.outcome(); ok || h == nil {
		return ErrFinished
	}
	return h.Send(ctx, msg)
}
