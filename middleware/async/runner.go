package async

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	cpmemory "github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Status is the state of a task: of a Handle's episode in Outcome.Status,
// and of the task in TaskRecord.Status.
type Status string

// The statuses of a task. StatusRunning and StatusWaiting do not end a
// task; the others, StatusLost included, are terminal: nothing resumes the
// task on its own, though an ended task may be continued (see Continuer).
const (
	// StatusRunning marks a task whose current episode has not stopped.
	StatusRunning Status = "running"
	// StatusWaiting marks a task that stopped to wait for a decision on the
	// actions in its Outcome.Pending (a tool call that needs approval when
	// nobody can be asked in-band). It has not ended: the host's decision
	// (Middleware.Resolve) starts its next episode (see Resolver).
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

// terminal reports whether s ends a task: every status but running and
// waiting.
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
	// this id. The local runner saves it with the parent agent's
	// Checkpointer, so a host thread of that name would collide with it:
	// never name a thread <thread>/async_N.
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
	// StatusCancelled. Once Done is closed, Stop returns nil and leaves Done
	// and Outcome unchanged; on a waiting episode the runner may then
	// release what it holds for the task (the middleware calls it when a
	// waiting task is cancelled: by cancel_task, by the end of the nested
	// run that started it, or by Close). Calling it again is safe. ctx
	// bounds the request itself (a remote runner that calls out), not the
	// wait for Done; an error means the request failed, and the middleware
	// still waits for Done.
	Stop(ctx context.Context) error
}

// Continuer is implemented by a Handle whose task can take a follow-up after
// it ended.
type Continuer interface {
	// Continue starts the next episode of the ended task with message, on
	// the same history, and returns its Handle; the receiver's Done stays
	// closed. It fails with ErrWaiting for a waiting episode and with
	// another error while the episode runs. At most one call per Handle
	// succeeds: a later or concurrent one fails with an error wrapping
	// ErrFinished and never starts a second episode (the next follow-up goes
	// to the new Handle). A failed call starts nothing, so it may be
	// retried. ctx carries the caller's values but the new episode must not
	// end with it (as for Runner.Start). It also carries the parent run's
	// current permission mode (FollowUpMode): the episode MUST run in
	// permission.Stricter of that mode and the task's own, so a parent
	// that entered plan mode cannot resume a task that writes. A Handle
	// that Reattacher.Attach returned for an ended record may stand on a
	// history that still holds a pending turn (the record alone cannot
	// tell a task cancelled while it waited from any other cancelled
	// one): Continue must abandon that turn, as agent.Agent.Run does,
	// and never run its pending actions.
	Continue(ctx context.Context, message string) (Handle, error)
}

// Resolver is implemented by a Handle whose task can be resumed after it
// stopped waiting.
type Resolver interface {
	// Resolve answers the waiting episode's Pending with decisions, one per
	// request, and returns the Handle of the next episode; the receiver's
	// Done stays closed. It fails with ErrNotWaiting when the episode did
	// not stop waiting. At most one call per Handle succeeds: a later or
	// concurrent one fails with an error wrapping ErrNotWaiting and never
	// starts a second episode. ctx is handled as by Continue, permission
	// mode included (FollowUpMode): the episode MUST run in
	// permission.Stricter of it and the task's own mode.
	Resolve(ctx context.Context, decisions []agent.Decision) (Handle, error)
}

// Reattacher is implemented by a Runner that can take over a task this
// process does not hold.
type Reattacher interface {
	// Attach returns a Handle for the task of rec, which this process does
	// not hold (after a restart, or after the middleware released it). For
	// a waiting or ended record it returns a Handle whose episode stopped:
	// Done is closed and Outcome mirrors the record (Status, Result as
	// Text, Error and Pending), so it can be continued or resolved. For a
	// running record, whose process went away, it goes on with the task:
	// a running Handle of a new episode, or a stopped waiting one when the
	// task waits. ctx may belong to no run (a host's Resolve after a
	// restart); every episode must then still run nested in the agent the
	// task was started from. It
	// fails with an error wrapping ErrCannotReattach when the task's
	// history is gone or the record cannot be taken over. The middleware
	// serializes Attach calls, so a runner need not handle two concurrent
	// calls for one record. ctx is handled as by Runner.Start; it carries
	// the parent run's current permission mode (FollowUpMode), which every
	// later episode of the Handle MUST apply as Continue does.
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

// localRunner runs tasks as subagents of the run that starts them. Each task
// runs on its own thread (Request.TaskThreadID) and is saved there, with the
// parent agent's Checkpointer or, when it has none, with fallback, a store of
// this process: a task can then be continued in this process even after the
// middleware released it. fallback's entry of a task is deleted once the
// task's record is pruned (see pruned); the parent's Checkpointer is never
// pruned.
type localRunner struct {
	specs    map[string]subagents.Subagent
	launcher *subagents.Launcher // built with inboxes as its extra
	inboxes  *inboxes
	timeout  time.Duration
	fallback *cpmemory.Checkpointer
	// onEvent is Config.OnEvent.
	onEvent func(threadID, taskID string, ev agent.Event)
	// bound returns the agent the middleware is bound to (see
	// Middleware.Bind); nil when none.
	bound func() *agent.Agent
}

var _ Reattacher = (*localRunner)(nil)

// newLocalRunner returns the local runner of specs; New builds it when
// Config.Runner is nil.
func newLocalRunner(specs map[string]subagents.Subagent, timeout time.Duration, onEvent func(string, string, agent.Event), bound func() *agent.Agent) (*localRunner, error) {
	// The inbox middleware is one pointer: every child, and their children,
	// share it, and the Launcher dedupes it by identity.
	boxes := newInboxes()
	l := &localRunner{specs: specs, inboxes: boxes, timeout: timeout, fallback: cpmemory.New(), onEvent: onEvent, bound: bound}
	launcher, err := subagents.NewLauncher(subagents.LauncherConfig{Extra: []agent.Middleware{boxes}, Checkpointer: l.checkpointer})
	if err != nil {
		return nil, err
	}
	l.launcher = launcher
	return l, nil
}

// checkpointer is the store the tasks started by parent's runs save with.
func (l *localRunner) checkpointer(parent *agent.Agent) checkpoint.Checkpointer {
	if cp := parent.Config().Checkpointer; cp != nil {
		return cp
	}
	return l.fallback
}

// pruned forgets the fallback entry of a task whose record was pruned; no
// one can continue it any more. The parent's Checkpointer is left alone.
func (l *localRunner) pruned(r TaskRecord) {
	if r.TaskThreadID != "" {
		_ = l.fallback.Delete(context.Background(), r.TaskThreadID) // absent when the parent saved it
	}
}

// followUpKey keys the followUp the middleware adds to the context of a
// Continue or Attach call.
type followUpKey struct{}

// followUp is what the middleware tells the local runner about the parent
// call that continues (or reattaches) a task.
type followUp struct {
	// mode is the parent run's current permission mode: the next episode
	// runs in the stricter of it and the task's own mode.
	mode permission.Mode
	// backend is the backend a reattached task runs on: the update_task
	// call's, the starting run's, or for a host's Resolve the thread's
	// (agent.Agent.ThreadBackend).
	backend backend.Backend
	// parent, when set, is the agent a reattached task runs under, at
	// Depth 1, whatever run ctx belongs to: the bound agent, for a host's
	// Resolve.
	parent *agent.Agent
}

// withFollowUp returns ctx carrying f.
func withFollowUp(ctx context.Context, f followUp) context.Context {
	return context.WithValue(ctx, followUpKey{}, f)
}

// FollowUpMode returns the parent run's current permission mode, which the
// middleware passes in the ctx of Continuer.Continue, Resolver.Resolve and
// Reattacher.Attach; ok is false for any other ctx. The episode it starts
// must run in permission.Stricter of this mode and the task's own: the
// middleware passes the mode, the runner enforces it. Since only the
// built-in runner is known to, the middleware refuses a follow-up or a
// decision through any other Runner while this mode is stricter than the
// parent's when it started the task (TaskRecord.StartMode).
func FollowUpMode(ctx context.Context) (permission.Mode, bool) {
	f, ok := followUpOf(ctx)
	if !ok || f.mode == "" {
		return "", false
	}
	return f.mode, true
}

// followUpOf returns the followUp of ctx, if any.
func followUpOf(ctx context.Context) (followUp, bool) {
	f, ok := ctx.Value(followUpKey{}).(followUp)
	return f, ok
}

// Start implements Runner: it prepares the subagent like the task tool does
// and runs it on req.TaskThreadID, saved (not ephemeral), on a context
// detached from the call, so the run keeps the call's values (its Depth is
// the parent's plus one) but not its cancellation.
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
	in.Ephemeral = false
	in.Depth = info.Depth + 1
	sess := &localSession{l: l, taskID: req.TaskID, parentThread: req.ThreadID, child: child, threadID: in.ThreadID, backend: in.Backend, agentName: in.AgentName, depth: in.Depth}
	return sess.start(ctx, in, nil)
}

// restartText is the message that continues a task whose process went away
// while it ran.
const restartText = "The process running this task restarted. Continue the task from where you left off."

// Attach implements Reattacher. It loads the task's thread from the
// checkpointer its episodes saved to: the parent agent's Checkpointer, else
// the runner's in-memory store, which a new process starts empty (the task
// then cannot be reattached). The parent agent is the one of the run ctx
// belongs to, else the agent the middleware is bound to (a host's Resolve
// after a restart); the middleware may name it. The task runs on the
// backend the middleware passed with ctx (the update_task call's, the
// starting run's, or the thread's own for a host's Resolve), else on the
// parent's BackendFactory(rec.ThreadID) or Backend, which a runner
// without the middleware's help can only guess: such a backend may not be
// the thread's (an Input.Backend a host passes per run) nor hold its
// synced workspace.
//
// From the loaded thread: a pending action gives a Handle whose episode
// stopped waiting on it, read from the thread in full, not from the
// record; an ended record gives a stopped Handle with the record's
// outcome, which can be continued; a running record (its process went
// away) starts a new episode from the saved State, told that the process
// restarted (the calls the crash cut off are answered as for any run, see
// agent.OrphanResolver). Every episode runs nested (agent.Input.Depth) in
// the stricter of the task's mode and FollowUpMode's. A task cancelled
// while it waited cannot be taken over (as the middleware refuses to
// continue one it holds).
func (l *localRunner) Attach(ctx context.Context, rec TaskRecord) (Handle, error) {
	if rec.TaskThreadID == "" {
		return nil, fmt.Errorf("%w: the record names no task thread", ErrCannotReattach)
	}
	parent, depth := l.parentOf(ctx)
	if parent == nil {
		return nil, fmt.Errorf("%w: no agent to run it with", ErrCannotReattach)
	}
	spec, ok := l.specs[rec.SubagentType]
	if !ok {
		return nil, fmt.Errorf("%w: unknown subagent type %q", ErrCannotReattach, rec.SubagentType)
	}
	f, _ := followUpOf(ctx)
	be := f.backend
	if be == nil {
		be = backendOf(parent, rec.ThreadID)
	}
	if be == nil {
		return nil, fmt.Errorf("%w: no backend to run it on", ErrCannotReattach)
	}
	child, err := l.launcher.Agent(parent, spec)
	if err != nil {
		return nil, err
	}
	s, v, err := l.checkpointer(parent).Load(ctx, rec.TaskThreadID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return nil, fmt.Errorf("%w: its thread %s is gone", ErrCannotReattach, rec.TaskThreadID)
	}
	if err != nil {
		return nil, err
	}
	sess := &localSession{l: l, taskID: rec.ID, parentThread: rec.ThreadID, child: child, threadID: rec.TaskThreadID, backend: be, agentName: spec.Name, depth: depth}
	out := Outcome{Status: rec.Status, Text: rec.Result, Error: rec.Error}
	intr, pending := agent.Pending(s)
	switch {
	case rec.Status == StatusCancelled && pending:
		// Cancelled while it waited: its pending actions never run.
		return nil, errCancelledWaiting
	case pending && !rec.Status.terminal():
		// The thread's, in full: the record's is a capped copy. A running
		// record whose task started waiting before its process went away
		// waits too.
		out = Outcome{Status: StatusWaiting, Pending: intr}
	case rec.Status == StatusWaiting:
		return nil, fmt.Errorf("%w: its thread %s holds no pending action", ErrCannotReattach, rec.TaskThreadID)
	case rec.Status == StatusRunning:
		in, err := sess.input(ctx, s, v, llm.UserText(restartText))
		if err != nil {
			return nil, err
		}
		return sess.start(ctx, in, nil)
	}
	done := make(chan struct{})
	close(done)
	return &localTask{
		sess: sess, stop: func(error) {}, done: done, inbox: &inbox{closed: true},
		out: out, last: s, version: v,
	}, nil
}

// parentOf returns the agent a task reattached with ctx runs under, and the
// Depth of its runs: the parent the middleware passed (followUp.parent), at
// Depth 1; else the agent of the run ctx belongs to, one level below that
// run; else the bound agent, at Depth 1.
func (l *localRunner) parentOf(ctx context.Context) (*agent.Agent, int) {
	if f, ok := followUpOf(ctx); ok && f.parent != nil {
		return f.parent, 1
	}
	if info, ok := agent.Info(ctx); ok && info.Agent != nil {
		return info.Agent, info.Depth + 1
	}
	if l.bound != nil {
		if a := l.bound(); a != nil {
			return a, 1
		}
	}
	return nil, 0
}

// backendOf is the backend parent's runs of threadID use when the run
// names none: BackendFactory(threadID), else Backend; nil when neither.
func backendOf(parent *agent.Agent, threadID string) backend.Backend {
	cfg := parent.Config()
	if cfg.BackendFactory != nil {
		return cfg.BackendFactory(threadID)
	}
	return cfg.Backend
}

// localSession is what every episode of one local task shares.
type localSession struct {
	l      *localRunner
	taskID string
	// parentThread is the thread of the run that started the task (its
	// record's ThreadID); threadID is the task's own thread.
	parentThread string
	child        *agent.Agent
	threadID     string
	backend      backend.Backend
	agentName    string
	// depth is the agent.Input.Depth of every episode: the starting run's
	// plus one, so an episode started outside a tool call (after a restart,
	// or by a host's decision) still runs nested.
	depth int
}

// input is the Input of an episode that continues the task from last (saved
// at version) with msgs: on a copy of last in the stricter of the task's
// permission mode and FollowUpMode's, nested, on the task's backend.
func (sess *localSession) input(ctx context.Context, last *state.State, version checkpoint.Version, msgs ...llm.Message) (agent.Input, error) {
	s := last.Clone()
	mode := sess.child.PermissionMode(s)
	if f, ok := followUpOf(ctx); ok && f.mode != "" {
		mode = permission.Stricter(f.mode, mode)
	}
	if err := agent.SetPermissionMode(s, mode); err != nil {
		return agent.Input{}, err
	}
	return agent.Input{
		ThreadID:  sess.threadID,
		State:     s,
		Version:   version,
		Messages:  msgs,
		Backend:   sess.backendFor(ctx),
		AgentName: sess.agentName,
		Depth:     sess.depth,
	}, nil
}

// start runs one episode of the task with in, on a context detached from
// ctx: it keeps ctx's values but not its cancellation. With decisions it
// resumes the interrupted State in.State holds (agent.Agent.ResumeWith).
func (sess *localSession) start(ctx context.Context, in agent.Input, decisions []agent.Decision) (*localTask, error) {
	box, err := sess.l.inboxes.open(sess.threadID)
	if err != nil {
		return nil, err
	}
	stopCtx, stop := context.WithCancelCause(context.WithoutCancel(ctx))
	runCtx, cancelTimeout := context.WithTimeout(stopCtx, sess.l.timeout)
	t := &localTask{sess: sess, stop: stop, done: make(chan struct{}), inbox: box}
	go func() {
		defer cancelTimeout()
		t.run(runCtx, in, decisions, func() int { return sess.l.inboxes.remove(sess.threadID) })
	}()
	return t, nil
}

// localTask is the Handle of one episode of a subagent running in this
// process.
type localTask struct {
	sess  *localSession
	stop  func(cause error)
	done  chan struct{}
	inbox *inbox

	mu       sync.Mutex
	progress string
	out      Outcome
	// last and version are the State the episode ended with and its
	// checkpoint version, which the next episode starts from.
	last    *state.State
	version checkpoint.Version
	// continued is set once a Continue or Resolve of this episode
	// succeeded or is under way.
	continued bool
}

var (
	_ Continuer = (*localTask)(nil)
	_ Resolver  = (*localTask)(nil)
)

// run consumes the subagent's stream to its end (of a resumption when
// decisions is not nil), passing every event to Config.OnEvent and keeping
// its recent text as progress, then closes the inbox (closeInbox returns how
// many messages were never read), publishes the outcome and closes done, in
// that order.
func (t *localTask) run(ctx context.Context, in agent.Input, decisions []agent.Decision, closeInbox func() int) {
	stream := t.sess.child.Run
	if decisions != nil {
		stream = func(ctx context.Context, in agent.Input) iter.Seq2[agent.Event, error] {
			return t.sess.child.ResumeWith(ctx, in, decisions)
		}
	}
	var final *agent.Final
	for ev := range stream(ctx, in) {
		t.sess.emit(ctx, ev)
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
	if final != nil {
		t.last, t.version = final.State, final.Version
	}
	t.mu.Unlock()
	t.stop(nil) // releases the context
	close(t.done)
}

// emit passes ev to Config.OnEvent, recovering a panic.
func (sess *localSession) emit(ctx context.Context, ev agent.Event) {
	if sess.l.onEvent == nil {
		return
	}
	defer recoverHook(ctx, "async: OnEvent panicked")
	sess.l.onEvent(sess.parentThread, sess.taskID, ev)
}

// Continue implements Continuer: the next episode runs the subagent on the
// task's thread again, from the State this one ended with, with message
// framed as an update. It runs in the stricter of the task's mode and the
// parent's current one, when the middleware passed it. Only the first call
// succeeds; later ones fail with ErrFinished.
func (t *localTask) Continue(ctx context.Context, message string) (Handle, error) {
	if !isClosed(t.done) {
		return nil, errStillRunning
	}
	t.mu.Lock()
	switch {
	case t.continued:
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: this episode was already continued", ErrFinished)
	case t.out.Status == StatusWaiting:
		t.mu.Unlock()
		return nil, ErrWaiting
	}
	t.continued = true
	last, version := t.last, t.version
	t.mu.Unlock()
	h, err := t.next(ctx, last, version, message)
	if err != nil {
		t.mu.Lock()
		t.continued = false
		t.mu.Unlock()
		return nil, err
	}
	return h, nil
}

// next starts the episode that follows this one.
func (t *localTask) next(ctx context.Context, last *state.State, version checkpoint.Version, message string) (Handle, error) {
	sess := t.sess
	if last == nil {
		// The run ended without a Final: start from its last save.
		info, ok := agent.Info(ctx)
		parent := sess.child
		if ok && info.Agent != nil {
			parent = info.Agent
		}
		var err error
		if last, version, err = sess.l.checkpointer(parent).Load(ctx, sess.threadID); err != nil {
			return nil, fmt.Errorf("async: the task has no history to continue: %w", err)
		}
	}
	in, err := sess.input(ctx, last, version, llm.UserText(updateText(message)))
	if err != nil {
		return nil, err
	}
	return sess.start(ctx, in, nil)
}

// backendFor is the backend an episode runs on: the task's own, or for a
// reattached task the one the middleware passed with ctx.
func (sess *localSession) backendFor(ctx context.Context) backend.Backend {
	if sess.backend != nil {
		return sess.backend
	}
	f, _ := followUpOf(ctx)
	return f.backend
}

// Resolve implements Resolver: the next episode resumes the subagent on the
// task's thread from the interrupted State this one ended with, answering
// its pending actions with decisions (see agent.Agent.Resume), in the
// stricter of the task's permission mode and FollowUpMode's: an approved
// write is still denied while the parent is in plan mode. The pending actions are read from that State,
// in full, not from the capped copy a record shows. Decisions that do not
// answer every pending action exactly once, with a type it allows, are
// refused and start nothing. Only the first successful call starts an
// episode; later ones fail with ErrNotWaiting.
func (t *localTask) Resolve(ctx context.Context, decisions []agent.Decision) (Handle, error) {
	if !isClosed(t.done) {
		return nil, fmt.Errorf("%w: the episode is still running", ErrNotWaiting)
	}
	t.mu.Lock()
	switch {
	case t.continued:
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: this episode was already resumed", ErrNotWaiting)
	case t.out.Status != StatusWaiting || t.last == nil:
		t.mu.Unlock()
		return nil, ErrNotWaiting
	}
	if _, ok := agent.Pending(t.last); !ok {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: the task's thread holds no pending action", ErrNotWaiting)
	}
	if err := agent.CheckDecisions(t.last, decisions); err != nil {
		t.mu.Unlock()
		return nil, fmt.Errorf("async: %w", err)
	}
	t.continued = true
	last, version := t.last, t.version
	t.mu.Unlock()
	sess := t.sess
	in, err := sess.input(ctx, last, version)
	var h Handle
	if err == nil {
		h, err = sess.start(ctx, in, slices.Clone(decisions))
	}
	if err != nil {
		t.mu.Lock()
		t.continued = false
		t.mu.Unlock()
		return nil, err
	}
	return h, nil
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
