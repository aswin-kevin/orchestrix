package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"time"

	"github.com/aswin-kevin/orchestrix/internal/sse"
)

// ErrStreamIdle reports that a model reply sent nothing for
// Config.StreamIdleTimeout. IsRetryable reports it as retryable, but a
// stream that has already delivered content is never replayed, so in
// practice only an idle reply that showed nothing is tried again (by a
// fallback chain).
var ErrStreamIdle = errors.New("llm: stream idle timeout")

// maxSSEEventBytes caps one event of a model stream. The largest events are
// final response snapshots, which repeat the whole reply.
const maxSSEEventBytes = 16 << 20

// sseEvent is one server-sent event.
type sseEvent = sse.Event

// readSSE yields the events of a text/event-stream body (see sse.Events). An
// event over maxSSEEventBytes or a read failure ends the sequence with an
// error. idle, when not nil, restarts whenever bytes arrive, so keep-alive
// comments count as activity, and is paused while the consumer handles an
// event, so it measures only the silence of the stream.
func readSSE(r io.Reader, idle *idleTimer) iter.Seq2[sseEvent, error] {
	return func(yield func(sseEvent, error) bool) {
		idle.resume()
		defer idle.pause()
		for ev, err := range sse.Events(idleReader{r, idle}, maxSSEEventBytes) {
			idle.pause()
			if err != nil {
				yield(sseEvent{}, fmt.Errorf("llm: read event stream: %w", err))
				return
			}
			if !yield(ev, nil) {
				return
			}
			idle.resume()
		}
	}
}

// streamReadError reports the cause callers test for when a stream read
// fails. A cancelled context surfaces from the transport as a wrapped read
// error, so the idle timeout that cancelled it, or else the context's own
// error, takes its place.
func streamReadError(ctx context.Context, err error) error {
	if cause := idleCause(ctx); cause != nil {
		return cause
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// idleCause returns the ErrStreamIdle that cancelled ctx, or nil.
func idleCause(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, ErrStreamIdle) {
		return cause
	}
	return nil
}

// idleTimer cancels the context of one model call, with an ErrStreamIdle
// cause, once it has run for the idle timeout without being paused or
// restarted. Its methods are called from one goroutine; a nil *idleTimer
// does nothing.
type idleTimer struct {
	d      time.Duration
	cancel context.CancelCauseFunc
	timer  *time.Timer
}

// watchIdle derives the context of one model call from ctx and returns its
// idle timer, which is nil when StreamIdleTimeout is disabled. The timer
// starts on its first resume, once the response headers have arrived, so
// HTTP-level retries are unaffected. release stops the timer and frees the
// context; call it when the call ends.
func (c *Config) watchIdle(ctx context.Context) (_ context.Context, idle *idleTimer, release func()) {
	d := c.streamIdleTimeout()
	if d <= 0 {
		return ctx, nil, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	idle = &idleTimer{d: d, cancel: cancel}
	return ctx, idle, func() {
		idle.pause()
		cancel(nil)
	}
}

// resume (re)starts the full idle timeout.
func (t *idleTimer) resume() {
	if t == nil {
		return
	}
	if t.timer == nil {
		t.timer = time.AfterFunc(t.d, func() {
			t.cancel(fmt.Errorf("%w: nothing received for %v", ErrStreamIdle, t.d))
		})
		return
	}
	t.timer.Reset(t.d)
}

// pause stops the timer until the next resume.
func (t *idleTimer) pause() {
	if t != nil && t.timer != nil {
		t.timer.Stop()
	}
}

// idleReader restarts idle whenever a read returns data: any bytes received
// count as activity.
type idleReader struct {
	r    io.Reader
	idle *idleTimer
}

func (r idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.idle.resume()
	}
	return n, err
}
