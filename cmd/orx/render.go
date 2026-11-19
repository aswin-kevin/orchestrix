package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/termsafe"
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
	// topAgent is the agent whose output is shown unprefixed; events of any
	// other agent are subagent output.
	topAgent string

	// mu guards midLine, textAgent and pending: the approval prompt arrives
	// from a tool goroutine while the main one renders events.
	mu sync.Mutex
	// midLine is true while streamed text has not yet been ended by a newline.
	midLine bool
	// textAgent is the agent whose text is streaming while midLine is set or
	// pending is not empty.
	textAgent string
	// pending holds the leading bytes of a UTF-8 rune split across textAgent's
	// deltas, until the rest arrives; see splitIncompleteRune.
	pending string
}

func newRenderer(stdout, stderr io.Writer) *renderer {
	return &renderer{stdout: stdout, stderr: &lockedWriter{w: stderr}, dim: isTerminal(stderr), topAgent: agent.DefaultAgentName}
}

// subagentIndent and the bracketed name mark output of a nested agent.
const subagentIndent = "  "

// prefix returns the line prefix for output of the named agent: empty for
// the top-level agent.
func (r *renderer) prefix(name string) string {
	if name == "" || name == r.topAgent {
		return ""
	}
	return subagentIndent + "[" + name + "] "
}

// prompts returns the writer for interactive prompts. It ends a streamed
// text line first, so a prompt never lands in the middle of a reply.
func (r *renderer) prompts() io.Writer { return promptWriter{r} }

type promptWriter struct{ r *renderer }

func (w promptWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	w.r.endText()
	return w.r.stderr.Write(p)
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
// Everything printed from an event (tool names, inputs, results, assistant
// text and errors) comes from the model or a tool, so it goes through
// termsafe.String; the renderer's own styling is added after.
func (r *renderer) render(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pre := r.prefix(ev.Agent)
	switch ev.Kind {
	case agent.KindText:
		r.streamText(ev.Agent, pre, ev.Text)
	case agent.KindToolUse:
		r.status(pre + fmt.Sprintf("⏺ %s(%s)", termsafe.String(ev.ToolName), compactJSON(ev.Input)))
	case agent.KindToolResult:
		if ev.ToolResult != nil {
			r.status(pre + "  ⎿ " + truncate(termsafe.String(firstLine(ev.ToolResult.Flatten())), maxLineRunes))
		}
	case agent.KindFinal:
		r.final(ev.Final)
	}
}

// streamText prints one text delta of the named agent. Each delta is escaped
// on its own (an escape sequence split across deltas still has its ESC in one
// of them), except for an incomplete rune at its end: those bytes are held
// back and joined to the agent's next delta, so a character split across
// deltas is not shown as escaped invalid bytes. Text of another agent, a
// status line, a prompt or the final event flush the held bytes, escaped.
func (r *renderer) streamText(agentName, pre, delta string) {
	if r.pending != "" && agentName != r.textAgent {
		r.endText()
	}
	complete, pending := splitIncompleteRune(r.pending + delta)
	r.pending = pending
	if pending != "" {
		r.textAgent = agentName
	}
	r.text(agentName, pre, termsafe.String(complete))
}

// splitIncompleteRune returns s without its trailing bytes that begin a UTF-8
// sequence still missing bytes (at most utf8.UTFMax-1), and those bytes.
// Bytes that cannot become a valid rune stay in complete.
func splitIncompleteRune(s string) (complete, pending string) {
	for i := len(s) - 1; i >= 0 && i >= len(s)-(utf8.UTFMax-1); i-- {
		if !utf8.RuneStart(s[i]) {
			continue
		}
		if !utf8.FullRuneInString(s[i:]) {
			return s[:i], s[i:]
		}
		break
	}
	return s, ""
}

// text streams s to stdout. Every line of a subagent's text starts with
// pre; text of a different agent than the one mid-line starts a new line.
func (r *renderer) text(agentName, pre, s string) {
	if s == "" {
		return
	}
	if r.midLine && agentName != r.textAgent {
		r.endText()
	}
	if pre != "" {
		if !r.midLine {
			s = pre + s
		}
		// Prefix each further line, but not the empty one after a final newline.
		body := strings.TrimSuffix(s, "\n")
		s = strings.ReplaceAll(body, "\n", "\n"+pre) + s[len(body):]
	}
	fmt.Fprint(r.stdout, s)
	r.midLine = !strings.HasSuffix(s, "\n")
	r.textAgent = agentName
}

// endText finishes a streamed text line so later output starts on a new
// one, first printing any held bytes of an incomplete rune, escaped. The
// caller holds mu.
func (r *renderer) endText() {
	if r.pending != "" {
		held := r.pending
		r.pending = ""
		r.text(r.textAgent, r.prefix(r.textAgent), termsafe.String(held))
	}
	if r.midLine {
		fmt.Fprintln(r.stdout)
		r.midLine = false
	}
}

// flush ends a streamed text line, printing any held bytes; a turn calls it
// on every exit, so they are neither lost nor joined to the next turn.
func (r *renderer) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endText()
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
		return fmt.Sprintf("stopped: %s: %s", f.Reason, termsafe.String(f.Err.Error()))
	}
	return fmt.Sprintf("stopped: %s", f.Reason)
}

func usageLine(u llm.Usage) string {
	return fmt.Sprintf("tokens: in %d · out %d · cache %d", u.Input, u.Output, u.CacheRead)
}

// compactJSON renders raw on one line, terminal-safe and limited to
// maxLineRunes. Valid JSON carries control characters only as \u escapes,
// but invalid input is shown as it came.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return truncate(termsafe.String(firstLine(string(raw))), maxLineRunes)
	}
	return truncate(termsafe.String(buf.String()), maxLineRunes)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// truncate shortens s to at most limit runes, marking the cut with an ellipsis.
func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-1]) + "…"
}
