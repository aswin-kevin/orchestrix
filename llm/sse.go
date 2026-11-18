package llm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"iter"
	"strings"
)

// sseEvent is one server-sent event.
type sseEvent struct {
	// Event is the value of the "event:" field, empty when absent.
	Event string
	// Data is the "data:" lines joined by newlines.
	Data string
}

// readSSE yields the events of a text/event-stream body. Comment lines and
// unknown fields are skipped, and an event still pending at EOF is emitted
// even without a terminating blank line. A read failure ends the sequence
// with an error.
func readSSE(r io.Reader) iter.Seq2[sseEvent, error] {
	return func(yield func(sseEvent, error) bool) {
		br := bufio.NewReader(r)
		var event string
		var data []string

		// flush emits the pending event, if it carries data, and reports
		// whether the consumer wants more.
		flush := func() bool {
			if len(data) == 0 {
				event = ""
				return true
			}
			ev := sseEvent{Event: event, Data: strings.Join(data, "\n")}
			event, data = "", nil
			return yield(ev, nil)
		}

		for {
			line, err := br.ReadString('\n')
			if line != "" || err == nil {
				line = strings.TrimRight(line, "\r\n")
				if line == "" {
					if !flush() {
						return
					}
				} else {
					field, value := splitSSELine(line)
					switch field {
					case "event":
						event = value
					case "data":
						data = append(data, value)
					}
				}
			}
			if err == io.EOF {
				flush()
				return
			}
			if err != nil {
				yield(sseEvent{}, fmt.Errorf("llm: read event stream: %w", err))
				return
			}
		}
	}
}

// splitSSELine splits "field: value" and drops the single optional space after
// the colon. Comment lines (leading colon) yield an empty field.
func splitSSELine(line string) (field, value string) {
	field, value, _ = strings.Cut(line, ":")
	return field, strings.TrimPrefix(value, " ")
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
