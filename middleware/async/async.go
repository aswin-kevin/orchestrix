// Package async runs subagents in the background. With it, the model starts a
// subagent with start_task and keeps working while it runs; it can look at a
// task's progress or wait for it (check_task), send it new instructions or,
// once it ended, a follow-up (update_task), cancel it (cancel_task) and list
// the tasks of the conversation (list_tasks). When a task ends, the next
// turn of the thread starts with a user message announcing it with an
// excerpt of its result, so the model learns of it without polling.
//
// A background subagent is started exactly like one of the task tool (see
// subagents.Launcher): the same specs, the stricter permission mode, the
// narrowed skills scope and the call's backend. By default it runs in this
// process; Config.Runner runs tasks elsewhere instead.
//
// A task runs in episodes, each a Handle: its start is the first, and a
// follow-up sent with update_task to an ended task starts the next, on the
// same history, when the Handle is a Continuer. The task keeps its id and
// is announced again when the episode ends. The local runner runs each task
// on a thread of its own, <parent thread>/<task id>, and saves it with the
// parent agent's Checkpointer: those threads are never deleted, by the
// middleware or anything else in it, and they show up among the store's
// threads (checkpoint.Lister), so never give a thread of your own such a
// name; delete them yourself once you no longer need them. Without a
// Checkpointer it keeps them in memory, for this process only, until the
// task's record is pruned (Config.MaxFinished); only those entries are ever
// deleted. A task the middleware released is taken over again through the
// Runner's Reattacher when a follow-up names it.
//
// A background run has nobody to ask for approval in-band: with an Approver
// on the parent agent the subagent inherits it and asks it, in-band, as a
// foreground one does. Without one, a subagent that needs a decision stops
// as StatusWaiting, with the actions it waits for in Outcome.Pending and
// TaskRecord.Pending. The parent model is told the task waits and on what,
// but it cannot answer: no tool approves another run's actions, update_task
// refuses a waiting task, and cancel_task only ends it. The decision belongs
// to the user, through the host: Config.OnSettled tells the host when a task
// starts waiting, and Resolve answers its pending actions and resumes it,
// still nested in the run that started it. A waiting task is never released
// nor forgotten (it does not count against Config.MaxFinished) until it is
// resolved or cancelled.
//
// A task's permission mode is fixed when an episode starts: for the first,
// the stricter of the parent's mode at that moment and the spec's; for a
// follow-up or a decision, the stricter of the parent's current mode and
// the task's own. A parent that enters plan mode or a stricter mode does not
// change the mode of an episode already running, but a follow-up it sends,
// or a decision the host gives after a turn of the parent's thread in that
// mode, runs in it. The middleware
// passes the parent's mode to the runner (FollowUpMode) and the runner
// enforces it: the built-in runner does, and since a custom Runner may not,
// a follow-up or decision through one is refused while the parent's mode is
// stricter than when it started the task (TaskRecord.StartMode). A
// background subagent's State is not merged into the parent's (the parent
// may have moved on by the time it ends); files are shared through the
// backend.
//
// Tasks belong to a thread, not to a run: start_task needs the run's
// agent.Input.ThreadID and fails in a run without one. A top-level run may
// end while its tasks keep running, and a later run on the thread is told
// when they end.
// A run about to end while its tasks run is reminded of them once
// (Config.DisableStopReminder). The reminder is a BeforeStop continuation:
// it uses one of the run's agent.MaxStopContinuations slots, shared with
// every other BeforeStop middleware (rubric, hooks Stop hooks), and, placed
// after them in the stack, it is consulted before them. A nested run's
// tasks are stopped when it ends, so a subagent's own tasks never outlive
// it. Close stops every task; call it (or Agent.Close) before the program
// exits.
//
// Task records live in the run's State (see Tasks); a run only reads,
// announces and cancels the tasks of its own thread. While tasks run, the
// system prompt lists them from those records, so the model still knows of
// them after a compaction.
//
// # After a restart
//
// A task outlives the process that ran it only through its records and its
// thread. When a run starts, every record of its thread that shows a task
// running which this process does not hold (it was saved before a restart)
// is taken over through the Runner's Reattacher, in the stricter of the
// task's permission mode and the parent's current one: the built-in runner
// loads the task's thread from the parent agent's Checkpointer and, when
// that thread holds a pending action, the task waits again; otherwise it
// continues in a new episode told that the process restarted. A task that
// cannot be taken over (no Checkpointer, its thread is gone, or the Runner
// is no Reattacher) is reported to the model as lost. Ended tasks are taken
// over when update_task names them. Resolve takes over a waiting task the
// process does not hold, from the State saved with the Checkpointer of the
// agent the middleware is bound to (see Bind): use one Middleware per
// top-level agent when Resolve must work across restarts. Resolve may go
// on with a task even when it fails with ErrNotWaiting (a task recorded
// running is reattached first). Every episode of a task taken over runs
// nested (agent.RunInfo.Depth at least 1), as one started by start_task
// does, though no tool call of the parent starts it, and on the run's
// backend (for Resolve, the thread's own, see agent.Agent.ThreadBackend).
// One live process must own a thread's tasks at a time: two processes
// that take over the same task both run it.
package async

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/internal/jobs"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Tool names.
const (
	ToolStart  = "start_task"
	ToolCheck  = "check_task"
	ToolUpdate = "update_task"
	ToolCancel = "cancel_task"
	ToolList   = "list_tasks"
)

// Defaults applied when the matching Config field is zero, and the fixed
// limits of the middleware.
const (
	// DefaultMaxRunning is how many tasks may run at once in one thread.
	DefaultMaxRunning = 4
	// DefaultMaxFinished is how many ended tasks are remembered per thread.
	DefaultMaxFinished = 50
	// DefaultTaskTimeout stops a task of the local runner that runs this long.
	DefaultTaskTimeout = 30 * time.Minute
	// DefaultMaxWait caps how long check_task waits for a task to end.
	DefaultMaxWait = 5 * time.Minute
	// DefaultStopWait bounds how long cancel_task and Close wait for
	// stopped tasks to end.
	DefaultStopWait = 5 * time.Second
	// DefaultMaxResultBytes caps, in bytes, the result excerpt shown in a
	// notification and by list_tasks.
	DefaultMaxResultBytes = 4_000
	// MaxRecordedResult caps, in bytes, TaskRecord.Result and
	// TaskRecord.Error: records live in State, which every checkpoint saves.
	MaxRecordedResult = 16 << 10
	// MaxCheckResultBytes caps, in bytes, the full result check_task shows
	// of an ended task; a longer one is cut, with a note saying so.
	MaxCheckResultBytes = 4 * MaxRecordedResult
	// DefaultProgressBytes is how much of its most recent text, in bytes, a
	// running task of the local runner reports as progress.
	DefaultProgressBytes = 2_000
	// IDPrefix starts every task id; the rest is the task's number in its
	// thread.
	IDPrefix = "async_"
	// NotificationMetaKey marks a task notification message: its
	// llm.Message.Meta holds the task's id under this key, and "async" under
	// agent.InjectedMetaKey.
	NotificationMetaKey = "orchestrix.async_notification"
)

// descriptionPreviewBytes caps, in bytes, the task description list_tasks
// shows.
const descriptionPreviewBytes = 120

// Config configures the async middleware.
type Config struct {
	// Subagents are the subagent types start_task can start, as in
	// subagents.Config; nil offers only the general-purpose subagent.
	Subagents []subagents.Subagent
	// DisableGeneralPurpose removes the built-in general-purpose subagent;
	// false offers it (unless a spec replaces it).
	DisableGeneralPurpose bool
	// GeneralPurposePrompt is the general-purpose subagent's system prompt;
	// "" means subagents.DefaultGeneralPurposePrompt.
	GeneralPurposePrompt string
	// Runner starts the tasks; nil runs them as subagents in this process,
	// each on its own thread saved with the agent's Checkpointer (see the
	// package doc): those threads are never deleted, only the in-memory
	// entries kept when the agent has no Checkpointer are pruned with their
	// records, so delete the task threads of the store yourself once no
	// longer needed. A task's thread is <parent thread>/async_N (IDPrefix
	// and a number): that shape is reserved, so never give a thread of
	// your own a segment of it after its first.
	Runner Runner
	// MaxRunning is how many tasks may run at once per thread; 0 means
	// DefaultMaxRunning. Negative values are rejected.
	MaxRunning int
	// MaxFinished is how many ended tasks are remembered per thread, and how
	// many announced records a run's State keeps; 0 means
	// DefaultMaxFinished. Negative values are rejected.
	MaxFinished int
	// TaskTimeout stops a task of the local runner that runs this long (it
	// then ends as StatusTimedOut); 0 means DefaultTaskTimeout. A custom
	// Runner applies its own limits. Negative values are rejected.
	TaskTimeout time.Duration
	// MaxDepth bounds nesting: a run at agent.RunInfo.Depth >= MaxDepth
	// cannot start tasks. 0 means subagents.DefaultMaxDepth. Negative values
	// are rejected.
	MaxDepth int
	// Prompt is the system prompt segment describing background tasks. nil
	// uses DefaultPrompt; a pointer to "" disables the segment.
	Prompt *string
	// DisableStopReminder turns off the reminder a run gets, once, when it
	// is about to end while tasks of its thread still run; false sends it.
	DisableStopReminder bool
	// OnSettled is called when an episode of a task stops: it ended, or it
	// now waits for a decision (rec.Pending set; see Resolve). A waiting
	// task that is cancelled (by cancel_task, Close or the end of the
	// nested run that started it) is reported once more, cancelled.
	// threadID is the thread of the run that started the task, and rec is
	// built from the task (its identity and the episode's outcome, capped
	// as records are) with Notified false; it is not the record in any
	// State. It runs after the outcome is published, on the goroutine that
	// watched the episode, or for a cancelled waiting task on a goroutine
	// of its own, so it should not block: a slow one delays the task's
	// next report, not cancel_task or Close; that cancelled report may thus
	// run after Close returned. It may call Resolve. A task
	// stopped as it started (Close raced start_task), which the parent
	// never recorded, is not reported. ctx is the context the episode was
	// started from, without its cancellation: the start_task or update_task
	// call's, carrying the parent run's agent.Info, or the ctx given to
	// Resolve, which may carry none. A panic is recovered and logged with
	// the logger of agent.Info(ctx), when ctx has one. nil calls nothing.
	OnSettled func(ctx context.Context, threadID string, rec TaskRecord)
	// OnEvent receives every event of every episode of a task the built-in
	// runner runs, KindFinal included, synchronously and in order, on the
	// goroutine running the episode: a slow OnEvent slows the task.
	// threadID is the thread of the run that started the task, as for
	// OnSettled, and taskID the task's id: ids repeat across threads, so
	// the pair names the task. A custom Runner does not feed it. A panic is
	// recovered and logged as for OnSettled. nil calls nothing.
	OnEvent func(threadID, taskID string, ev agent.Event)
}

// Name is the name of the async middleware.
const Name = "async"

// Middleware is the async middleware. Unlike most middleware it holds state
// of its own, the live tasks, which it shares between every run and agent it
// is used with; that state is safe for concurrent use. Create it with New.
type Middleware struct {
	specs    map[string]subagents.Subagent
	order    []string
	runner   Runner
	maxDepth int
	prompt   string
	remind   bool
	jobs     *jobs.Manager[*task]
	tools    []tool.Tool
	// acc reads the records; its Pruned hook lets the runner forget
	// what it keeps for a pruned task.
	acc jobs.Accessors[TaskRecord]
	// attachMu serializes Reattacher.Attach calls.
	attachMu sync.Mutex
	// onSettled is Config.OnSettled.
	onSettled func(ctx context.Context, threadID string, rec TaskRecord)
	// bound is the first agent the middleware was bound to (see Bind).
	bound atomic.Pointer[agent.Agent]
}

var (
	_ agent.ToolProvider = (*Middleware)(nil)
	_ agent.BeforeAgent  = (*Middleware)(nil)
	_ agent.BeforeModel  = (*Middleware)(nil)
	_ agent.ModelWrapper = (*Middleware)(nil)
	_ agent.BeforeStop   = (*Middleware)(nil)
	_ agent.AfterAgent   = (*Middleware)(nil)
	_ agent.Binder       = (*Middleware)(nil)
)

// remindedKey is set once a run was reminded of its running tasks.
var remindedKey = state.NewKey[bool]("async.reminded", state.Private[bool]())

// New returns the async middleware. It fails for an invalid subagent spec
// (see subagents.ResolveSpecs), a negative limit, or a configuration that
// leaves no subagent to start.
func New(cfg Config) (*Middleware, error) {
	for _, f := range []struct {
		name     string
		negative bool
	}{
		{"MaxRunning", cfg.MaxRunning < 0},
		{"MaxFinished", cfg.MaxFinished < 0},
		{"TaskTimeout", cfg.TaskTimeout < 0},
		{"MaxDepth", cfg.MaxDepth < 0},
	} {
		if f.negative {
			return nil, fmt.Errorf("async: %s must not be negative", f.name)
		}
	}
	list, err := subagents.ResolveSpecs(subagents.Config{
		Subagents:             cfg.Subagents,
		DisableGeneralPurpose: cfg.DisableGeneralPurpose,
		GeneralPurposePrompt:  cfg.GeneralPurposePrompt,
	})
	if err != nil {
		return nil, fmt.Errorf("async: %w", err)
	}
	m := &Middleware{
		specs:     make(map[string]subagents.Subagent, len(list)),
		order:     make([]string, len(list)),
		runner:    cfg.Runner,
		maxDepth:  positiveOr(cfg.MaxDepth, subagents.DefaultMaxDepth),
		prompt:    DefaultPrompt,
		remind:    !cfg.DisableStopReminder,
		onSettled: cfg.OnSettled,
		jobs: jobs.NewManager[*task](IDPrefix, jobs.Limits{
			MaxRunning:  positiveOr(cfg.MaxRunning, DefaultMaxRunning),
			MaxFinished: positiveOr(cfg.MaxFinished, DefaultMaxFinished),
		}),
	}
	for i, s := range list {
		m.specs[s.Name], m.order[i] = s, s.Name
	}
	if m.runner == nil {
		l, err := newLocalRunner(m.specs, positiveOr(cfg.TaskTimeout, DefaultTaskTimeout), cfg.OnEvent, m.bound.Load)
		if err != nil {
			return nil, fmt.Errorf("async: %w", err)
		}
		m.runner = l
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	m.acc = recordAccessors
	if p, ok := m.runner.(interface{ pruned(TaskRecord) }); ok {
		m.acc.Pruned = p.pruned
	}
	// A waiting task is kept until it is resolved or cancelled.
	m.jobs.SetPinned((*task).waiting)
	m.tools = m.buildTools()
	return m, nil
}

func positiveOr[T int | time.Duration](v, fallback T) T {
	if v > 0 {
		return v
	}
	return fallback
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Bind implements agent.Binder. The middleware keeps the first agent it is
// bound to, the one the host built, and ignores later binds: every subagent
// built from that agent (by start_task, the task tool or any Launcher) goes
// through agent.New with the same middleware. Resolve uses that agent to
// take over a task after a restart, with its Checkpointer and backend. Use
// one async Middleware per top-level agent when Resolve must work across
// restarts: a Middleware shared by several agents resolves through the
// first one only. It never fails.
func (m *Middleware) Bind(a *agent.Agent) error {
	m.bound.CompareAndSwap(nil, a)
	return nil
}

// Close implements agent.Closer, so Agent.Close stops the tasks: it stops
// every task of every thread and waits up to DefaultStopWait for them to
// end, or until ctx's deadline when that comes sooner; the stop requests,
// sent before that wait, may add up to backend.DefaultKillGrace. Tasks
// cannot be started afterwards, and the records of the tasks it stopped
// show them as cancelled (as the runner reports them). It returns an error
// when a task had not ended by then (ctx's error when ctx ended). Calling
// it more than once is safe.
//
// Close does not wait for Config.OnSettled: a report in flight, or the
// cancelled report of a waiting task Close ended, may run after it
// returns, so an OnSettled that uses resources the host frees after Close
// must check for that itself.
func (m *Middleware) Close(ctx context.Context) error {
	var waiting []*jobs.Job[*task]
	for _, j := range m.jobs.All() {
		if j.Handle.cancelWaiting() {
			waiting = append(waiting, j)
		}
	}
	stopWaiting(ctx, waiting)
	return m.jobs.Close(ctx, DefaultStopWait)
}

// Resolve answers the pending actions of task taskID of thread threadID,
// which waits for a decision (StatusWaiting), with decisions, one per
// action of its TaskRecord.Pending matched by ToolUseID, and starts its next
// episode through the Runner's Resolver. It is the host's answer, on the
// user's behalf: the parent model has no tool for it. The episode runs as
// the task's earlier ones did: nested in the run that started the task
// (agent.RunInfo.Depth at least 1), on the backend of its start, in the
// stricter of the task's permission mode and the parent's as last seen at a
// turn of its thread (see FollowUpMode; through a custom Runner, a parent
// mode stricter than at the task's start refuses the decision). ctx only
// bounds the call (OnSettled gets it, detached, when the episode stops).
// The parent's record shows the task running at the parent's next turn, and
// its end is announced as usual.
//
// A task this process does not hold, such as one recorded before a
// restart, is first taken over: Resolve loads threadID's State from the
// Checkpointer of the agent the middleware is bound to (see Bind), finds
// the task's record there and reattaches it through the Runner's
// Reattacher, with the parent's permission mode as saved in that State; it
// then runs under that agent, nested at Depth 1, on the thread's own
// backend (agent.Agent.ThreadBackend: its synced workspace restored). A
// record that still shows the task running (its parent's run ended before
// the task started waiting) is reattached as the thread's next run would
// reattach it: when the task's history shows it waiting it is resolved,
// and otherwise it goes on in a new episode, held by this process, and
// Resolve fails with ErrNotWaiting: Resolve may thus continue a task even
// when it fails. Only a top-level thread's tasks are taken over this way:
// threadID naming a task's own thread (<thread>/async_N) fails with
// ErrCannotReattach, since that task's next run takes over its tasks, under
// the task's agent and depth. One live process must own a thread's tasks
// at a time: two that take over the same task both run it. A task taken
// over runs on the backend the bound agent resolves for the thread
// (BackendFactory, else Config.Backend): a host that only passes
// agent.Input.Backend per run has neither, and Resolve then fails with
// ErrCannotReattach ("no backend to run it on"). Taking over reads and,
// for a synced workspace, restores the thread's saved State and backend, so
// it must not run while a run of the same thread is live: resolve between
// runs.
//
// threadID is the thread of the run that started the task, as OnSettled
// reports it; task threads (<thread>/async_N, see Config.Runner) are
// reserved for the tasks themselves.
//
// Only one decision per wait wins: a concurrent or later Resolve, a
// cancel_task or an update_task fails, so the approved actions run at most
// once. Resolve fails with ErrUnknownTask when neither this process nor
// the thread's saved State knows the task, with ErrNotWaiting when it is
// not waiting (or another decision was taken), with an error wrapping
// ErrCannotReattach when the task must be taken over and the Runner
// cannot reattach it, or when the middleware is bound to no agent or the
// agent has no Checkpointer (the error then also wraps ErrUnknownTask:
// nothing says the task exists), with an error when the thread already runs
// Config.MaxRunning tasks, and with the runner's error, such as a
// decision that does not answer every pending action exactly once with a
// type it allows (agent.ErrMissingDecision, agent.ErrInvalidDecision); the
// task then still waits. Test ErrCannotReattach before ErrUnknownTask: an
// error that wraps both says the task could not be looked for.
func (m *Middleware) Resolve(ctx context.Context, threadID, taskID string, decisions []agent.Decision) error {
	j := m.jobs.Get(threadID, taskID)
	if j == nil {
		var err error
		if j, err = m.reattachSaved(ctx, threadID, taskID); err != nil {
			return err
		}
	}
	if !j.Handle.waiting() {
		return fmt.Errorf("%w: %s", ErrNotWaiting, taskID)
	}
	mode := j.Handle.lastParentMode()
	if err := m.checkFollowUpMode(mode, j.Handle.identity().StartMode); err != nil {
		return fmt.Errorf("async: resolve task %s: %w", taskID, err)
	}
	f := followUp{mode: mode}
	_, err := m.jobs.Restart(threadID, taskID, func() error {
		return j.Handle.resolveWith(withFollowUp(j.Handle.base, f), ctx, decisions)
	})
	var limit *jobs.LimitError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotWaiting):
		return err
	case errors.Is(err, jobs.ErrUnknownJob):
		// Another decision restarted the task first.
		return fmt.Errorf("%w: %s", ErrNotWaiting, taskID)
	case errors.Is(err, jobs.ErrShutdown):
		return errors.New("async: background tasks are shut down")
	case errors.As(err, &limit):
		return fmt.Errorf("async: thread %s already runs %d tasks; resolve task %s once one ends", threadID, limit.Limit, taskID)
	}
	return fmt.Errorf("async: resolve task %s: %w", taskID, err)
}

// reattachSaved takes over task taskID of threadID, which this process does
// not hold, from the record in the thread's State saved with the bound
// agent's Checkpointer (see Resolve).
func (m *Middleware) reattachSaved(ctx context.Context, threadID, taskID string) (*jobs.Job[*task], error) {
	a := m.bound.Load()
	if a == nil {
		return nil, fmt.Errorf("%w: %w: task %s: this process does not hold it and the middleware is bound to no agent", ErrUnknownTask, ErrCannotReattach, taskID)
	}
	if isTaskThread(threadID) {
		// A task's own tasks belong to its runs: they are reattached by
		// the task's next run, under the task's agent and depth.
		return nil, fmt.Errorf("%w: task %s: thread %s is a background task's own thread; its tasks are taken over by that task's next run", ErrCannotReattach, taskID, threadID)
	}
	cp := a.Config().Checkpointer
	if cp == nil {
		return nil, fmt.Errorf("%w: %w: task %s: this process does not hold it and the agent has no Checkpointer", ErrUnknownTask, ErrCannotReattach, taskID)
	}
	s, _, err := cp.Load(ctx, threadID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownTask, taskID)
	}
	if err != nil {
		return nil, fmt.Errorf("async: resolve task %s: load thread %s: %w", taskID, threadID, err)
	}
	records := Tasks(s)
	i := slices.IndexFunc(records, func(r TaskRecord) bool { return r.ID == taskID && r.ThreadID == threadID })
	if i < 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownTask, taskID)
	}
	r := records[i]
	if r.Status != StatusWaiting && r.Status != StatusRunning {
		return nil, fmt.Errorf("%w: %s", ErrNotWaiting, taskID)
	}
	if _, ok := m.runner.(Reattacher); !ok {
		return nil, fmt.Errorf("%w: task %s: the runner cannot reattach tasks", ErrCannotReattach, taskID)
	}
	f := followUp{mode: a.PermissionMode(s), parent: a}
	if err := m.checkFollowUpMode(f.mode, r.StartMode); err != nil {
		return nil, fmt.Errorf("async: resolve task %s: %w", taskID, err)
	}
	// The thread's own backend, its synced workspace restored, so the
	// task's files add to the thread's saved ones.
	if f.backend, err = a.ThreadBackend(ctx, threadID, s); err != nil {
		return nil, fmt.Errorf("async: resolve task %s: %w", taskID, err)
	}
	j, err := m.reattach(withFollowUp(ctx, f), r)
	if err != nil {
		return nil, fmt.Errorf("async: resolve task %s: %w", taskID, err)
	}
	return j, nil
}

// checkFollowUpMode refuses a follow-up or a decision through a Runner
// other than the built-in one while the parent's current mode is stricter
// than its mode at the task's start: only the built-in runner is known to
// apply FollowUpMode.
func (m *Middleware) checkFollowUpMode(current, start permission.Mode) error {
	if _, local := m.runner.(*localRunner); local || current == "" {
		return nil
	}
	if permission.Stricter(current, start) == permission.Stricter(start, start) {
		return nil
	}
	if start == "" {
		start = permission.ModeDefault
	}
	return fmt.Errorf("the conversation is now in %s mode, stricter than when the task started (%s), and its runner cannot be made to apply it; start a new task instead", current, start)
}

// settled is every task's settledFunc: it reports the episode of t that
// stopped with out, at ended, to Config.OnSettled.
func (m *Middleware) settled(ctx context.Context, t *task, out Outcome, ended time.Time) {
	if m.onSettled == nil {
		return
	}
	rec := t.identity()
	if rec.ID == "" {
		return // never recorded: its start failed
	}
	if j := m.jobs.Get(rec.ThreadID, rec.ID); j != nil && j.Handle == t {
		out, ended = m.stoppedOutcome(j, out, ended)
	}
	applyOutcome(&rec, out, ended)
	defer recoverHook(ctx, "async: OnSettled panicked")
	m.onSettled(ctx, rec.ThreadID, rec)
}

// recoverHook recovers a panic of a host hook and logs it as msg with the
// logger of agent.Info(ctx) (a discarding one when ctx has no run). It must
// be deferred directly.
func recoverHook(ctx context.Context, msg string) {
	if v := recover(); v != nil {
		info, _ := agent.Info(ctx)
		info.Logger().Error(msg, "err", fmt.Errorf("%v", v))
	}
}

// Drain implements agent.Drainer: Agent.Close stops the background tasks
// this way before it closes the resources they use (MCP sessions, jobs,
// backends). It is Close: the tasks are cancelled and waited for within
// ctx, and none can be started afterwards.
func (m *Middleware) Drain(ctx context.Context) error { return m.Close(ctx) }

// BeforeModel implements agent.BeforeModel: every task of the run's thread
// that ended, or started waiting, since the last turn is announced with one
// user message (a task notification, marked with NotificationMetaKey) that
// says how it stopped, with an excerpt of its result. Records of other
// threads are left alone, so a run never takes another thread's
// notifications. A waiting task is announced once but stays unsettled: it
// is still held, so neither its record nor its live task is dropped.
//
// Before that, the live tasks of a thread whose tasks all ended and were
// announced in earlier turns are released from memory; check_task then
// reports the recorded outcome.
func (m *Middleware) BeforeModel(ctx context.Context, s *state.State) error {
	threadID, _ := agent.ThreadID(ctx)
	if threadID != "" {
		m.jobs.Release(threadID)
	}
	var mode permission.Mode
	if info, ok := agent.Info(ctx); ok && info.Agent != nil {
		mode = info.Agent.PermissionMode(s)
	}
	var notices []llm.Message
	m.updateRecords(s, threadID, func(r *TaskRecord) {
		if j := m.jobs.Get(r.ThreadID, r.ID); j != nil && mode != "" {
			j.Handle.setParentMode(mode) // for a host's decision
		}
		switch {
		case r.Notified || r.Status == StatusRunning:
		case r.Status == StatusWaiting:
			notices = append(notices, notification(*r))
			r.Notified = true
			if j := m.jobs.Get(r.ThreadID, r.ID); j != nil {
				j.Handle.sawWait()
			}
		default:
			notices = append(notices, notification(*r))
			m.markNotified(r)
		}
	})
	s.AppendMessages(notices...)
	return nil
}

// BeforeAgent implements agent.BeforeAgent: each run may be reminded of its
// running tasks once, and the running tasks of the run's thread that this
// process does not hold, typically recorded before a restart, are taken
// over through the Runner's Reattacher (see "After a restart" in the
// package doc); one that cannot be is reported lost.
func (m *Middleware) BeforeAgent(ctx context.Context, s *state.State) error {
	remindedKey.Delete(s)
	m.reattachRunning(ctx, s)
	return nil
}

// reattachRunning takes over, through the Runner's Reattacher, every record
// of the run's thread in s that shows a task running which this process
// does not hold, as the parent's current permission mode allows (see
// checkFollowUpMode): the task waits again when its history holds a pending
// action, and otherwise goes on in a new episode. A task that cannot be
// taken over, or any task when the Runner is no Reattacher, is reported as
// lost at the run's first turn.
func (m *Middleware) reattachRunning(ctx context.Context, s *state.State) {
	info, ok := agent.Info(ctx)
	if !ok || info.ThreadID == "" {
		return
	}
	if _, ok := m.runner.(Reattacher); !ok {
		return
	}
	// The task works on the run's own backend, as it did when started.
	f := followUp{backend: info.Backend()}
	if info.Agent != nil {
		f.mode = info.Agent.PermissionMode(s)
	}
	records, _ := tasksKey.Get(s)
	for _, r := range jobs.ThreadRecords(records, info.ThreadID, recordAccessors) {
		if r.Status != StatusRunning || m.jobs.Get(r.ThreadID, r.ID) != nil {
			continue
		}
		err := m.checkFollowUpMode(f.mode, r.StartMode)
		if err == nil {
			_, err = m.reattach(withFollowUp(ctx, f), r)
		}
		if err != nil {
			info.Logger().Warn("async: background task not reattached; it is reported lost", "task", r.ID, "err", err)
		}
	}
}

// WrapModel implements agent.ModelWrapper. While tasks of the run's thread
// run, it appends a list of them (id, subagent, how long it has run and the
// start of its description, in a <background_tasks> block) after the host's
// system segments, then the prompt; both stay outside the cached prefix. The list
// comes from the records in State, not from the messages, so it survives a
// compaction that summarized the start_task calls away.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	var add []string
	if req.State != nil {
		records, _ := tasksKey.Get(req.State)
		threadID, _ := agent.ThreadID(ctx)
		if list := runningList(jobs.ThreadRecords(records, threadID, recordAccessors), time.Now()); list != "" {
			add = append(add, list)
		}
	}
	if m.prompt != "" {
		add = append(add, m.prompt)
	}
	if len(add) > 0 {
		req.Request.System = append(slices.Clone(req.Request.System), add...)
	}
	return next(ctx, req)
}

// BeforeStop implements agent.BeforeStop: a run about to end while tasks of
// its thread still run is reminded of them, once per run, unless
// Config.DisableStopReminder is set.
func (m *Middleware) BeforeStop(ctx context.Context, s *state.State, _ *llm.Response) (string, error) {
	if reminded, _ := remindedKey.Get(s); !m.remind || reminded {
		return "", nil
	}
	threadID, _ := agent.ThreadID(ctx)
	ids := runningIDs(m.updateRecords(s, threadID, nil))
	if len(ids) == 0 {
		return "", nil
	}
	remindedKey.Set(s, true)
	return fmt.Sprintf(stopReminder, strings.Join(ids, ", ")), nil
}

// AfterAgent implements agent.AfterAgent: a nested run's tasks are stopped
// and forgotten when it ends, so a subagent's background tasks never outlive
// it; their records end cancelled, and as seen, since nobody is left to be
// told, and so do the records of its waiting tasks (nobody is left to
// decide) and of its tasks that ended unannounced. A
// top-level run's tasks keep running for later runs of the thread. Only
// tasks of the run's own thread are stopped.
//
// At every depth the thread's live tasks are then released from memory
// once none runs there and every ended one was announced, so idle threads
// and finished nested ones hold no memory.
func (m *Middleware) AfterAgent(ctx context.Context, s *state.State) error {
	info, ok := agent.Info(ctx)
	if !ok || info.ThreadID == "" {
		return nil
	}
	if info.Depth > 0 {
		m.endNested(ctx, s, info.ThreadID)
	}
	m.jobs.Release(info.ThreadID)
	return nil
}

// endNested stops the running tasks of a nested run's thread, cancels its
// waiting ones and marks every ended task of it as seen.
func (m *Middleware) endNested(ctx context.Context, s *state.State, threadID string) {
	records, _ := tasksKey.Get(s)
	if ids := runningIDs(jobs.ThreadRecords(records, threadID, recordAccessors)); len(ids) > 0 {
		m.jobs.StopAndForget(ctx, threadID, ids, DefaultStopWait, func() {
			m.updateRecords(s, threadID, func(r *TaskRecord) {
				if slices.Contains(ids, r.ID) {
					m.markNotified(r)
				}
			})
		})
	}
	var waiting []*jobs.Job[*task]
	var resumed []string
	m.updateRecords(s, threadID, func(r *TaskRecord) {
		if r.Status == StatusWaiting {
			// A task a decision is resuming now runs, and its thread's
			// runs are over: it is stopped below.
			list, ok := m.cancelWaiting(r)
			waiting = append(waiting, list...)
			if !ok {
				resumed = append(resumed, r.ID)
			}
		}
		if r.terminal() && !r.Notified {
			m.markNotified(r)
		}
	})
	stopWaiting(ctx, waiting)
	if len(resumed) > 0 {
		m.jobs.StopAndForget(ctx, threadID, resumed, DefaultStopWait, func() {
			m.updateRecords(s, threadID, func(r *TaskRecord) {
				if slices.Contains(resumed, r.ID) {
					// A decision that had claimed the task may not have
					// committed its episode: the record would still
					// wait. Either way the task is stopped for good.
					if !r.terminal() {
						r.Status, r.Pending, r.Ended = StatusCancelled, nil, time.Now()
					}
					m.markNotified(r)
				}
			})
		})
	}
}

// cancelWaiting ends the record of a waiting task as cancelled and seen, and
// marks its job notified. It claims the task entry, so no follow-up or
// decision starts an episode of it, and returns the job whose Handle the
// caller must stop (see stopWaiting) once the State is no longer being
// updated; none when this process does not hold it. ok is false, and r is
// left alone, when the process holds the task but it no longer waits: a
// decision is resuming it.
func (m *Middleware) cancelWaiting(r *TaskRecord) (list []*jobs.Job[*task], ok bool) {
	j := m.jobs.Get(r.ThreadID, r.ID)
	if j != nil && !j.Handle.cancelWaiting() {
		return nil, false
	}
	r.Status, r.Pending, r.Ended = StatusCancelled, nil, time.Now()
	m.markNotified(r)
	if j != nil {
		list = append(list, j)
	}
	return list, true
}

// stopWaiting calls Stop on the Handles of cancelled waiting tasks, all at
// once, each bounded by DefaultStopWait or ctx's deadline when that comes
// sooner: nothing runs, but the runner may release what it holds for the
// task. Each task is then reported to Config.OnSettled as cancelled, on a
// goroutine of its own: a slow OnSettled delays neither the caller nor the
// other stops.
func stopWaiting(ctx context.Context, list []*jobs.Job[*task]) {
	var wg sync.WaitGroup
	for _, j := range list {
		wg.Go(func() {
			defer func() { go j.Handle.settledCancelled() }()
			h := j.Handle.handle()
			if h == nil {
				return
			}
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultStopWait)
			defer cancel()
			if deadline, ok := ctx.Deadline(); ok {
				var cancelAt context.CancelFunc
				stopCtx, cancelAt = context.WithDeadline(stopCtx, deadline)
				defer cancelAt()
			}
			_ = h.Stop(stopCtx) // the record is cancelled either way
		})
	}
	wg.Wait()
}

// runningIDs returns the ids of the running records.
func runningIDs(records []TaskRecord) []string {
	var ids []string
	for _, r := range records {
		if r.Status == StatusRunning {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// markNotified records, in r and on its job, that the model was told the
// task ended.
func (m *Middleware) markNotified(r *TaskRecord) {
	r.Notified = true
	m.jobs.MarkNotified(r.ThreadID, r.ID)
}
