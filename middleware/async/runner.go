package async

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/tool"
)

// The values of Outcome.Status and TaskRecord.Status.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	// StatusCancelled marks a task stopped by cancel_task or Shutdown.
	StatusCancelled = "cancelled"
	// StatusTimedOut marks a task stopped at its time limit.
	StatusTimedOut = "timed_out"
	// StatusBlocked marks a task that needed a decision it could not ask
	// for: a background run has no one to ask in-band.
	StatusBlocked = "blocked"
	// StatusLost marks a task recorded as running that this process does not
	// know, typically because the State was saved before a restart.
	StatusLost = "lost"
)

// Request describes a task to start.
type Request struct {
	// TaskID is the task's id, IDPrefix+N; never empty.
	TaskID string
	// ThreadID is the thread of the run that starts the task; never empty.
	ThreadID string
	// SubagentType is one of the subagent types the middleware offers.
	SubagentType string
	// Description is the task as the model wrote it.
	Description string
	// Call is the start_task call (its State and Backend); never nil. A
	// remote runner usually needs only the fields above. It belongs to the
	// call: keep nothing from it after Start returns but what the task must
	// share, such as the backend.
	Call *tool.Call
}

// Outcome is how a task ended.
type Outcome struct {
	// Status is one of the Status constants other than StatusRunning and
	// StatusLost; an empty or unknown one is recorded as StatusFailed.
	Status string
	// Text is the task's final answer; empty when it gave none.
	Text string
	// Error says why a task that did not complete ended; empty otherwise.
	Error string
	// Usage is the tokens the task used; zero when unknown.
	Usage llm.Usage
}

// Handle is one started task.
type Handle interface {
	// Done is closed once the task has ended and its Outcome is set.
	Done() <-chan struct{}
	// Outcome returns how the task ended; ok is false until Done is closed.
	Outcome() (out Outcome, ok bool)
	// Progress returns recent output of a running task; "" when there is
	// none or the runner reports none.
	Progress() string
	// Send delivers a message to the running task, which should read it
	// before its next step; ErrFinished (or an error wrapping it) once the
	// task ended, another error when it cannot take messages.
	Send(message string) error
	// Stop asks the task to end; it is a no-op once the task ended. The task
	// should then end promptly with StatusCancelled.
	Stop() error
}

// Runner starts background tasks; the default runs them as subagents in this
// process. ctx is the start_task call's context: it carries the run's
// agent.Info but is cancelled when the call returns, so a Runner must detach.
// Start may block (a remote runner may call out); it must return once the
// task is started, not when it ends.
type Runner interface {
	Start(ctx context.Context, req Request) (Handle, error)
}

// errNoHandle is the start error of a Runner that returned neither a Handle
// nor an error.
var errNoHandle = errors.New("the runner returned no task")

// errStopped is the cancel cause of a local task stopped on purpose.
var errStopped = errors.New("async: task stopped")

// noTextResult is the result of a task that completed without text.
const noTextResult = "(the background task returned no text)"

// localRunner runs tasks as subagents of the run that starts them.
type localRunner struct {
	specs    map[string]subagents.Subagent
	launcher *subagents.Launcher // built with inboxes as its extra
	inboxes  *inboxes
	timeout  time.Duration
}

// Start implements Runner: it prepares the subagent like the task tool does
// and runs it on a context detached from the call, so the run keeps the
// call's values (its Depth is the parent's plus one) but not its
// cancellation.
func (l *localRunner) Start(ctx context.Context, req Request) (Handle, error) {
	info, ok := agent.Info(ctx)
	if !ok {
		return nil, errors.New("a background task can only be started inside an agent run")
	}
	spec, ok := l.specs[req.SubagentType]
	if !ok {
		return nil, fmt.Errorf("unknown subagent type %q", req.SubagentType)
	}
	child, err := l.launcher.Agent(info.Agent, spec)
	if err != nil {
		return nil, err
	}
	in, err := l.launcher.Input(ctx, child, spec, req.Description, req.TaskID, req.Call)
	if err != nil {
		return nil, err
	}
	box, err := l.inboxes.open(in.ThreadID)
	if err != nil {
		return nil, err
	}
	stopCtx, stop := context.WithCancelCause(context.WithoutCancel(ctx))
	runCtx, cancelTimeout := context.WithTimeout(stopCtx, l.timeout)
	t := &localTask{stop: stop, done: make(chan struct{}), inbox: box}
	go func() {
		defer cancelTimeout()
		t.run(runCtx, child, in, func() int { return l.inboxes.remove(in.ThreadID) })
	}()
	return t, nil
}

// localTask is the Handle of a subagent running in this process.
type localTask struct {
	stop  func(cause error)
	done  chan struct{}
	inbox *inbox

	mu       sync.Mutex
	progress string
	out      Outcome
}

// run consumes the subagent's stream to its end, keeping its recent text as
// progress, then closes the inbox (closeInbox returns how many messages were
// never read), publishes the outcome and closes done, in that order.
func (t *localTask) run(ctx context.Context, child *agent.Agent, in agent.Input, closeInbox func() int) {
	var final *agent.Final
	for ev := range child.Run(ctx, in) {
		switch {
		case ev.Kind == agent.KindFinal:
			final = ev.Final
		case ev.Kind == agent.KindText && ev.ParentToolID == "":
			t.addProgress(ev.Text)
		}
	}
	out := localOutcome(final, errors.Is(context.Cause(ctx), errStopped), errors.Is(ctx.Err(), context.DeadlineExceeded))
	out = withLateUpdates(out, closeInbox())
	t.mu.Lock()
	t.out = out
	t.mu.Unlock()
	t.stop(nil) // releases the context
	close(t.done)
}

// localOutcome maps a subagent's Final to the task's outcome. A run that
// completed (or reached its own turn or time limit) completed, even when a
// stop raced its end.
func localOutcome(final *agent.Final, stopped, timedOut bool) Outcome {
	if final == nil {
		return Outcome{Status: StatusFailed, Error: "the subagent ended without a result"}
	}
	out := Outcome{Usage: final.Usage}
	switch {
	case final.Reason == agent.ReasonCompleted:
		out.Status, out.Text = StatusCompleted, finalText(final)
	case final.Reason == agent.ReasonMaxTurns || final.Reason == agent.ReasonTimeout:
		out.Status, out.Text = StatusCompleted, fmt.Sprintf("%s\n[stopped: %s]", finalText(final), final.Reason)
	case final.Reason == agent.ReasonInterrupted:
		out.Status, out.Error = StatusBlocked, "needs approval for: "+strings.Join(pendingTools(final), ", ")
	case stopped:
		out.Status = StatusCancelled
	case timedOut:
		out.Status, out.Error = StatusTimedOut, "the task did not finish in time"
	default:
		out.Status, out.Error = StatusFailed, fmt.Sprintf("%s: %v", final.Reason, final.Err)
	}
	return out
}

// finalText is the subagent's last reply, or a note that it gave none.
func finalText(final *agent.Final) string {
	if final.Text == "" {
		return noTextResult
	}
	return final.Text
}

// pendingTools lists, once each, the tools an interrupted run waits for a
// decision on.
func pendingTools(final *agent.Final) []string {
	var names []string
	if intr, ok := agent.Pending(final.State); ok {
		for _, r := range intr.Requests {
			if !slices.Contains(names, r.Tool) {
				names = append(names, r.Tool)
			}
		}
	}
	if len(names) == 0 {
		return []string{"(unknown)"}
	}
	return names
}

// addProgress appends text to the progress, keeping its last
// DefaultProgressBytes bytes.
func (t *localTask) addProgress(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.progress = tail(t.progress+text, DefaultProgressBytes)
}

// Done implements Handle.
func (t *localTask) Done() <-chan struct{} { return t.done }

// Outcome implements Handle.
func (t *localTask) Outcome() (Outcome, bool) {
	select {
	case <-t.done:
	default:
		return Outcome{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out, true
}

// Progress implements Handle.
func (t *localTask) Progress() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.progress
}

// Send implements Handle: the subagent reads message before its next model
// call, or before it would end.
func (t *localTask) Send(message string) error { return t.inbox.send(message) }

// Stop implements Handle: it cancels the subagent's run.
func (t *localTask) Stop() error {
	t.stop(errStopped)
	return nil
}

// tail returns the last limit bytes of s at most, starting on a rune
// boundary.
func tail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[len(s)-limit:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}
