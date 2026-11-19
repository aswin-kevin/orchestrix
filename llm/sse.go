package llm

import (
	"context"
	"fmt"
	"io"
	"iter"

	"github.com/aswin-kevin/orchestrix/internal/sse"
)

// maxSSEEventBytes caps one event of a model stream. The largest events are
// final response snapshots, which repeat the whole reply.
const maxSSEEventBytes = 16 << 20

// sseEvent is one server-sent event.
type sseEvent = sse.Event

// readSSE yields the events of a text/event-stream body (see sse.Events). An
// event over maxSSEEventBytes or a read failure ends the sequence with an
// error.
func readSSE(r io.Reader) iter.Seq2[sseEvent, error] {
	return func(yield func(sseEvent, error) bool) {
		for ev, err := range sse.Events(r, maxSSEEventBytes) {
			if err != nil {
				yield(sseEvent{}, fmt.Errorf("llm: read event stream: %w", err))
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// streamReadError reports the cause callers test for when a stream read
// fails. A cancelled context surfaces from the transport as a wrapped read
// error, so the context's own error takes its place.
func streamReadError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}
