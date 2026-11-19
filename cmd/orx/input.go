package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
)

// lineResult is one line read from the input, or the error that ended it.
type lineResult struct {
	text string
	err  error
}

// lineReader turns a blocking input stream into a channel of lines so that a
// reader can also wait for context cancellation. The REPL loop and the
// approver both take their lines from one lineReader: two readers on the same
// stream would each swallow input meant for the other.
//
// A single goroutine does the blocking reads. It lives until the input ends
// (EOF or an error), which is fine for a CLI whose process exits soon after;
// it holds at most one unread line while nobody is receiving.
type lineReader struct {
	lines chan lineResult
}

func newLineReader(r io.Reader) *lineReader {
	lr := &lineReader{lines: make(chan lineResult)}
	go lr.readAll(bufio.NewReader(r))
	return lr
}

// readAll sends every line, then the terminating error, then closes the
// channel. A final line without a trailing newline is sent before the error.
func (lr *lineReader) readAll(r *bufio.Reader) {
	defer close(lr.lines)
	for {
		text, err := r.ReadString('\n')
		if text != "" {
			lr.lines <- lineResult{text: text}
		}
		if err != nil {
			lr.lines <- lineResult{err: err}
			return
		}
	}
}

// next returns the next line without its newline. It returns io.EOF once the
// input has ended and ctx.Err() when ctx is cancelled first; a line that
// arrives after cancellation stays available for the next call.
func (lr *lineReader) next(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res, ok := <-lr.lines:
		if !ok {
			return "", io.EOF
		}
		return trimNewline(res.text), res.err
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// idleWatching is called once readIdle watches for Ctrl-C; tests replace it.
var idleWatching = func() {}

// readIdle prints the idle prompt to out and reads the next line, holding
// the approver's prompt lock so that a question still being asked is
// answered first. Ctrl-C there cancels the read, so the REPL returns and the
// deferred shutdown runs; the default handler would kill the process and
// orphan background jobs.
func readIdle(ctx context.Context, in *lineReader, prompt *sync.Mutex, out io.Writer) (string, error) {
	prompt.Lock()
	defer prompt.Unlock()
	fmt.Fprint(out, replPrompt)
	ctx, stop := notifyInterrupt(ctx)
	defer stop()
	idleWatching()
	line, err := in.next(ctx)
	if err == nil {
		interrupted.Store(false) // the user has moved on
	}
	return line, err
}
