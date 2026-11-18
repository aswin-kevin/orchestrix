package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
)

const (
	// maxLineRunes caps the tool input and the tool result line shown to the
	// user, so one large payload cannot flood the terminal.
	maxLineRunes = 120

	ansiDim   = "\x1b[2m"
	ansiReset = "\x1b[0m"
)

// renderer prints a run's events: assistant text goes to stdout so it can be
// piped, everything else (tool activity, usage) goes to stderr.
type renderer struct {
	stdout, stderr io.Writer
	// dim wraps stderr lines in ANSI dim; set only when stderr is a terminal.
	dim bool
	// midLine is true while streamed text has not yet been ended by a newline.
	midLine bool
}

func newRenderer(stdout, stderr io.Writer) *renderer {
	return &renderer{stdout: stdout, stderr: &lockedWriter{w: stderr}, dim: isTerminal(stderr)}
}

// lockedWriter serializes writes. The approval prompt is written from the
// agent's tool goroutine while the renderer writes tool lines from the main
// one, and an arbitrary io.Writer is not safe for that.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// isTerminal reports whether w is a character device such as a terminal.
// Anything that is not an *os.File (a buffer in tests) is not a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// render prints one event. Thinking and progress events are not shown.
func (r *renderer) render(ev agent.Event) {
	switch ev.Kind {
	case agent.KindText:
		r.text(ev.Text)
	case agent.KindToolUse:
		r.status(fmt.Sprintf("⏺ %s(%s)", ev.ToolName, compactJSON(ev.Input)))
	case agent.KindToolResult:
		if ev.ToolResult != nil {
			r.status("  ⎿ " + truncate(firstLine(ev.ToolResult.Flatten()), maxLineRunes))
		}
	case agent.KindFinal:
		r.final(ev.Final)
	}
}

func (r *renderer) text(s string) {
	fmt.Fprint(r.stdout, s)
	if s != "" {
		r.midLine = !strings.HasSuffix(s, "\n")
	}
}

// endText finishes a streamed text line so later output starts on a new one.
func (r *renderer) endText() {
	if r.midLine {
		fmt.Fprintln(r.stdout)
		r.midLine = false
	}
}

// status prints one line of tool activity to stderr, dimmed on a terminal.
func (r *renderer) status(line string) {
	r.endText()
	if r.dim {
		line = ansiDim + line + ansiReset
	}
	fmt.Fprintln(r.stderr, line)
}

func (r *renderer) final(f *agent.Final) {
	if f.Reason != agent.ReasonCompleted {
		r.status(stopLine(f))
	}
	r.status(usageLine(f.Usage))
}

func stopLine(f *agent.Final) string {
	if f.Err != nil {
		return fmt.Sprintf("stopped: %s: %v", f.Reason, f.Err)
	}
	return fmt.Sprintf("stopped: %s", f.Reason)
}

func usageLine(u llm.Usage) string {
	return fmt.Sprintf("tokens: in %d · out %d · cache %d", u.Input, u.Output, u.CacheRead)
}

// compactJSON renders raw on one line, limited to maxLineRunes.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return truncate(firstLine(string(raw)), maxLineRunes)
	}
	return truncate(buf.String(), maxLineRunes)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// truncate shortens s to at most max runes, marking the cut with an ellipsis.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
