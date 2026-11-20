package async

import (
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
// has one.
type task struct {
	done chan struct{}

	mu      sync.Mutex
	h       Handle // nil until attached
	stopped bool   // Stop came before attach
	out     Outcome
	ended   time.Time
}

func newTask() *task { return &task{done: make(chan struct{})} }

// Done implements jobs.Handle.
func (t *task) Done() <-chan struct{} { return t.done }

// Stop implements jobs.Handle: it stops the attached Handle, or the one
// attached later.
func (t *task) Stop() error {
	t.mu.Lock()
	h := t.h
	t.stopped = true
	t.mu.Unlock()
	if h == nil {
		return nil
	}
	return h.Stop()
}

// attach hands t the started Handle and follows it until it ends.
func (t *task) attach(h Handle) {
	t.mu.Lock()
	t.h = h
	stop := t.stopped
	t.mu.Unlock()
	if stop {
		_ = h.Stop() // the outcome reports what the stop achieved
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

// finish publishes out, normalized, and then closes done.
func (t *task) finish(out Outcome) {
	switch out.Status {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusTimedOut, StatusBlocked:
	default:
		msg := fmt.Sprintf("the runner reported status %q", out.Status)
		if out.Error != "" {
			msg += ": " + out.Error
		}
		out.Status, out.Error = StatusFailed, msg
	}
	t.mu.Lock()
	t.out, t.ended = out, time.Now()
	t.mu.Unlock()
	close(t.done)
}

// outcome returns how the task ended and when; ok is false while it runs.
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

// send delivers msg to the attached Handle; ErrFinished once the task ended.
func (t *task) send(msg string) error {
	t.mu.Lock()
	h := t.h
	t.mu.Unlock()
	if _, _, ok := t.outcome(); ok || h == nil {
		return ErrFinished
	}
	return h.Send(msg)
}
