package async

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Status is the state of a task: of a Handle's episode in Outcome.Status,
// and of the task in TaskRecord.Status.
type Status string

// The statuses of a task. StatusRunning, StatusWaiting and StatusLost never
// end a task for good; the others are terminal.
const (
	// StatusRunning marks a task whose current episode has not stopped.
	StatusRunning Status = "running"
	// StatusWaiting marks a task that stopped to wait for a decision on the
	// actions in its Outcome.Pending (a tool call that needs approval when
	// nobody can be asked in-band). It has not ended: the decision starts
	// its next episode (see Resolver).
	StatusWaiting Status = "waiting"
	// StatusCompleted marks a task that finished its work.
	StatusCompleted Status = "completed"
	// StatusFailed marks a task that ended on an error, including a refusal
	// of the model running it.
	StatusFailed Status = "failed"
	// StatusCancelled marks a task stopped by cancel_task or Close.
	StatusCancelled Status = "cancelled"
	// StatusTimedOut marks a task stopped at its time limit.
	StatusTimedOut Status = "timed_out"
	// StatusLost marks a task recorded as running that this process does not
	// know, typically because the State was saved before a restart.
	StatusLost Status = "lost"
)

// terminal reports whether s ends a task for good: every status but running
// and waiting (nothing in this process can resume a lost task).
func (s Status) terminal() bool { return s != StatusRunning && s != StatusWaiting }

// Errors of the async contract. A Runner's Handle reports ErrFinished (or an
// error wrapping it); the others are reported by the middleware and by the
// optional interfaces' implementations.
var (
	// ErrFinished is the error of a message sent to an episode that
	// stopped, by Handle.Send or update_task.
	ErrFinished = errors.New("async: task already finished")
	// ErrUnknownTask reports a task id the middleware has no record of.
	ErrUnknownTask = errors.New("async: unknown task")
	// ErrNotWaiting reports a decision for a task that is not waiting.
	ErrNotWaiting = errors.New("async: task is not waiting")
	// ErrWaiting reports a message or follow-up for a task that waits for
	// a decision, which must come first.
	ErrWaiting = errors.New("async: task is waiting for a decision")
	// ErrCannotReattach reports a task this process cannot take over from
	// its record (see Reattacher).
	ErrCannotReattach = errors.New("async: task cannot be reattached")
)

// Request describes a task to start.
type Request struct {
	// TaskID is the task's id, IDPrefix+N; never empty.
	TaskID string
	// ThreadID is the thread of the run that starts the task; never empty.
	ThreadID string
	// TaskThreadID is the thread the task should run on, ThreadID+"/"+TaskID;
	// never empty. A runner that keeps the task's history (to continue it,
	// resume it after a decision or reattach it after a restart) keys it by
	// this id.
	TaskThreadID string
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

// Outcome is how an episode of a task stopped.
type Outcome struct {
	// Status is StatusWaiting or a terminal status (StatusCompleted,
	// StatusFailed, StatusCancelled, StatusTimedOut). Any other value, empty
	// included, is recorded as StatusFailed.
	Status Status
	// Text is the task's final answer; empty when it gave none.
	Text string
	// Error says why a task that did not complete ended; empty otherwise.
	Error string
	// Usage is the tokens the episode used; zero when unknown.
	Usage llm.Usage
	// Pending is what a waiting task needs a decision on; set exactly when
	// Status is StatusWaiting, with at least one request. A waiting Outcome
	// without it is recorded as StatusFailed, and it is dropped from any
	// other Outcome.
	Pending *agent.Interrupt
}

// Handle is one episode of a task: from its start (or a Continue or Resolve)
// until it ends or starts waiting. Interfaces never gain methods;
// capabilities are the optional Continuer and Resolver, which a Handle may
// also implement. Every method is safe for concurrent use, and the
// middleware may call any of them from any goroutine.
//
// An episode stops once: Done closes, Outcome turns ok, and both stay so. A
// later episode of the same task is a new Handle (see Continuer and
// Resolver); a Handle is never reopened. The package asynctest checks these
// rules.
type Handle interface {
	// Done is closed once the episode stopped, because the task ended or
	// because it now waits, and its Outcome is set; it stays closed. Every
	// call returns the same channel.
	Done() <-chan struct{}
	// Outcome returns how the episode stopped; ok is false until Done is
	// closed. Once ok, it returns the same Outcome on every call.
	Outcome() (out Outcome, ok bool)
	// Progress returns recent output of a running episode; "" when there is
	// none or the runner reports none. It must not block.
	Progress() string
	// Send delivers a message to the running episode, which should read it
	// before its next step. It returns ErrFinished (or an error wrapping
	// it) once Done is closed, and another error when the task cannot take
	// messages. ctx bounds the delivery itself (a remote runner that calls
	// out), not the task's reading of the message.
	Send(ctx context.Context, message string) error
	// Stop asks the episode to end; the task should then end promptly with
	// StatusCancelled. It is a no-op returning nil once Done is closed, and
	// calling it again is safe. ctx bounds the request itself (a remote
	// runner that calls out), not the wait for Done; an error means the
	// request failed, and the middleware still waits for Done.
	Stop(ctx context.Context) error
}

// Continuer is implemented by a Handle whose task can take a follow-up after
// it ended.
type Continuer interface {
	// Continue starts the next episode of the ended task with message, on
	// the same history, and returns its Handle; the receiver's Done stays
	// closed. It fails with ErrWaiting for a waiting episode and with
	// another error while the episode runs. ctx carries the caller's values
	// but the new episode must not end with it (as for Runner.Start).
	Continue(ctx context.Context, message string) (Handle, error)
}

// Resolver is implemented by a Handle whose task can be resumed after it
// stopped waiting.
type Resolver interface {
	// Resolve answers the waiting episode's Pending with decisions, one per
	// request, and returns the Handle of the next episode; the receiver's
	// Done stays closed. It fails with ErrNotWaiting when the episode did
	// not stop waiting. ctx is handled as by Continue.
	Resolve(ctx context.Context, decisions []agent.Decision) (Handle, error)
}

// Reattacher is implemented by a Runner that can take over a task this
// process does not hold.
type Reattacher interface {
	// Attach returns a Handle for the task of rec, which this process does
	// not hold (after a restart, or after the middleware released it). It
	// fails with an error wrapping ErrCannotReattach when the task's
	// history is gone. ctx is handled as by Runner.Start.
	Attach(ctx context.Context, rec TaskRecord) (Handle, error)
}

// Runner starts background tasks; the default runs them as subagents in this
// process. ctx is the start_task call's context: it carries the run's
// agent.Info but is cancelled when the call returns, so a Runner must detach.
// Start may block (a remote runner may call out); it must return once the
// task is started, not when it ends. It is called concurrently, for tasks of
// any thread. A Runner may also implement Reattacher, and its Handles
// Continuer and Resolver.
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
// and runs it on req.TaskThreadID, on a context detached from the call, so
// the run keeps the call's values (its Depth is the parent's plus one) but
// not its cancellation.
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
	in.ThreadID = req.TaskThreadID
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
// stop raced its end. An interrupted run waits for a decision on its pending
// actions. A refusal failed with the child's reply as the error.
func localOutcome(final *agent.Final, stopped, timedOut bool) Outcome {
	if final == nil {
		return Outcome{Status: StatusFailed, Error: "the subagent ended without a result"}
	}
	out := Outcome{Usage: final.Usage}
	switch {
	case final.Reason == agent.ReasonCompleted:
		out.Status, out.Text = StatusCompleted, finalText(final)
	case final.Reason == agent.ReasonMaxTurns || final.Reason == agent.ReasonTokenLimit || final.Reason == agent.ReasonTimeout:
		out.Status, out.Text = StatusCompleted, fmt.Sprintf("%s\n[stopped: %s]", finalText(final), final.Reason)
	case final.Reason == agent.ReasonInterrupted:
		if intr, ok := agent.Pending(final.State); ok && len(intr.Requests) > 0 {
			out.Status, out.Pending = StatusWaiting, intr
		} else {
			out.Status, out.Error = StatusFailed, "the subagent was interrupted with no pending action"
		}
	case final.Reason == agent.ReasonRefusal:
		out.Status, out.Error = StatusFailed, "refused"
		if final.Text != "" {
			out.Error += ": " + final.Text
		}
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
// call, or before it would end. Queuing it never blocks, so ctx is unused.
func (t *localTask) Send(_ context.Context, message string) error { return t.inbox.send(message) }

// Stop implements Handle: it cancels the subagent's run; once the run ended
// the cancel is a no-op.
func (t *localTask) Stop(context.Context) error {
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
