// Package sse reads server-sent event streams (text/event-stream bodies) with
// a cap on the size of one event, so a misbehaving server cannot make a
// reader buffer without bound. It is shared by the model providers and the
// MCP HTTP transport.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
)

// Event is one dispatched server-sent event.
type Event struct {
	// Event is the "event:" field; empty when absent, which means the
	// default type ("message").
	Event string
	// Data is the event's "data:" lines joined by "\n".
	Data string
	// ID is the stream's last event ID (the latest "id:" field so far, which
	// carries over to later events); empty when none was sent.
	ID string
}

// ErrTooLarge reports an event, or a single line, longer than the reader's
// limit.
var ErrTooLarge = errors.New("sse: event too large")

// Events yields the events of r. Lines end with LF or CRLF. Comment lines,
// unknown fields and events without data lines are skipped, and an event
// still pending at EOF is emitted even without a terminating blank line.
//
// max (positive) caps the bytes of one event's field lines, and of any single
// line: going over it ends the sequence with an error wrapping ErrTooLarge,
// before the oversized line is held in memory whole. A read failure ends the
// sequence with the read error.
func Events(r io.Reader, max int) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		br := bufio.NewReader(r)
		var (
			eventType, lastID string
			data              strings.Builder
			hasData           bool
			size              int // bytes of the pending event's field lines
		)
		for {
			line, err := readLine(br, max-size)
			if err == io.EOF {
				if hasData {
					yield(Event{Event: eventType, Data: data.String(), ID: lastID}, nil)
				}
				return
			}
			if errors.Is(err, ErrTooLarge) {
				err = fmt.Errorf("%w: over %d bytes", ErrTooLarge, max)
			}
			if err != nil {
				yield(Event{}, err)
				return
			}

			if line == "" {
				if hasData && !yield(Event{Event: eventType, Data: data.String(), ID: lastID}, nil) {
					return
				}
				eventType, hasData, size = "", false, 0
				data.Reset()
				continue
			}
			field, value := splitLine(line)
			switch field {
			case "event":
				eventType = value
			case "data":
				if hasData {
					data.WriteByte('\n')
				}
				data.WriteString(value)
				hasData = true
			case "id":
				if !strings.ContainsRune(value, 0) { // the spec ignores ids holding NUL
					lastID = value
				}
			default:
				continue // comments and unknown fields do not count toward the event
			}
			size += len(line)
		}
	}
}

// readLine returns the next line without its LF or CRLF terminator; a final
// line without one is returned too, and io.EOF after it. A line longer than
// limit bytes fails with ErrTooLarge as soon as that much has been read.
func readLine(br *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		content := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(content) > limit {
			return "", ErrTooLarge
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err == io.EOF && len(line) > 0:
			return string(content), nil
		case err != nil:
			return "", err
		}
		return string(content), nil
	}
}

// splitLine splits "field: value" and drops the single optional space after
// the colon. A line without a colon is a field with an empty value; a comment
// line (leading colon) yields an empty field.
func splitLine(line string) (field, value string) {
	field, value, _ = strings.Cut(line, ":")
	return field, strings.TrimPrefix(value, " ")
}
