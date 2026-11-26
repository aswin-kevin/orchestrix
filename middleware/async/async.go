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
// parent agent's Checkpointer: those threads are never deleted by the
// middleware, and they show up among the store's threads (checkpoint.Lister),
// so never give a thread of your own such a name. Without a Checkpointer it
// keeps them in memory, for this process only, until the task's record is
// pruned (Config.MaxFinished). A task the middleware released is taken over
// again through the Runner's Reattacher when a follow-up names it.
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
// them after a compaction. Running tasks are not re-attached in a new
// process: a record of a running task this process does not know is
// reported to the model as lost.
package async

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
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
	// package doc): the middleware never deletes those threads.
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
	// State. It runs on the goroutine that watched the episode, after the
	// outcome is published (for a cancel, on the canceller's), so it must
	// not block; it may call Resolve. ctx is the context the episode was
	// started from, without its cancellation: the start_task or update_task
	// call's, carrying the parent run's agent.Info, or the ctx given to
	// Resolve, which may carry none. A panic is recovered and logged with
	// the logger of agent.Info(ctx), when ctx has one. nil calls nothing.
	OnSettled func(ctx context.Context, threadID string, rec TaskRecord)
	// OnEvent receives every event of every episode of a task the built-in
	// runner runs, KindFinal included, synchronously and in order, on the
	// goroutine running the episode: a slow OnEvent slows the task. taskID
	// is the task's id; ids repeat across threads. A custom Runner does not
	// feed it. A panic is recovered and logged as for OnSettled. nil calls
	// nothing.
	OnEvent func(taskID string, ev agent.Event)
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
}

var (
	_ agent.ToolProvider = (*Middleware)(nil)
	_ agent.BeforeAgent  = (*Middleware)(nil)
	_ agent.BeforeModel  = (*Middleware)(nil)
	_ agent.ModelWrapper = (*Middleware)(nil)
	_ agent.BeforeStop   = (*Middleware)(nil)
	_ agent.AfterAgent   = (*Middleware)(nil)
)

// remindedKey is set once a run was reminded of its running tasks.
var remindedKey = state.NewKey[bool]("async.reminded", state.Private[bool]())

// New returns the async middleware. It fails for an invalid subagent spec
// (see subagents.ResolveSpecs), a negative limit, or a configuration that
// leaves no subagent to start.
func New(cfg Config) (*Middleware, error) {
	if cfg.MaxRunning < 0 || cfg.MaxFinished < 0 || cfg.TaskTimeout < 0 || cfg.MaxDepth < 0 {
		return nil, errors.New("async: MaxRunning, MaxFinished, TaskTimeout and MaxDepth must not be negative")
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
		m.runner = newLocalRunner(m.specs, positiveOr(cfg.TaskTimeout, DefaultTaskTimeout), cfg.OnEvent)
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

// Close implements agent.Closer, so Agent.Close stops the tasks: it stops
// every task of every thread and waits up to DefaultStopWait for them to
// end, or until ctx's deadline when that comes sooner. Tasks cannot be
// started afterwards, and the records of the tasks it stopped show them as
// cancelled (as the runner reports them). It returns an error when a task
// had not ended by then (ctx's error when ctx ended). Calling it more than
// once is safe.
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
// the task's earlier ones did: nested in the run that started the task, on
// the backend of its start, in the stricter of the task's permission mode
// and the parent's as last seen at a turn of its thread (see FollowUpMode;
// through a custom Runner, a parent mode stricter than at the task's start
// refuses the decision). ctx only bounds the call (OnSettled gets it,
// detached, when the episode stops).
// The parent's record shows the task running at the parent's next turn, and
// its end is announced as usual.
//
// Only one decision per wait wins: a concurrent or later Resolve, a
// cancel_task or an update_task fails, so the approved actions run at most
// once. Resolve fails with ErrUnknownTask when this process does not hold
// the task, with ErrNotWaiting when it is not waiting (or another decision
// was taken), with an error when the thread already runs
// Config.MaxRunning tasks, and with the runner's error, such as a
// decision that does not answer every pending action exactly once with a
// type it allows (agent.ErrMissingDecision, agent.ErrInvalidDecision); the
// task then still waits. A waiting task this process does not hold, such
// as one recorded before a restart, fails with ErrUnknownTask.
func (m *Middleware) Resolve(ctx context.Context, threadID, taskID string, decisions []agent.Decision) error {
	j := m.jobs.Get(threadID, taskID)
	if j == nil {
		return fmt.Errorf("%w: %s", ErrUnknownTask, taskID)
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
// user message (see notification). Records of other threads are left alone,
// so a run never takes another thread's notifications. A waiting task is
// announced once but stays unsettled: its job is not marked notified, so
// neither its record nor the thread's entry is dropped.
//
// Before that, a thread whose tasks all ended and were announced in earlier
// turns is released (see jobs.Manager.Release); check_task then reports the
// recorded outcome.
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
		default:
			notices = append(notices, notification(*r))
			m.markNotified(r)
		}
	})
	s.AppendMessages(notices...)
	return nil
}

// BeforeAgent implements agent.BeforeAgent: each run may be reminded of its
// running tasks once.
func (m *Middleware) BeforeAgent(_ context.Context, s *state.State) error {
	remindedKey.Delete(s)
	return nil
}

// WrapModel implements agent.ModelWrapper. While tasks of the run's thread
// run, it appends a list of them (see runningList) after the host's system
// segments, then the prompt; both stay outside the cached prefix. The list
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
// At every depth the thread's entry is then released once no task runs
// there and every ended one was announced (see jobs.Manager.Release), so
// idle threads and finished nested ones hold no memory.
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
// task. Each task is then reported to Config.OnSettled as cancelled.
func stopWaiting(ctx context.Context, list []*jobs.Job[*task]) {
	var wg sync.WaitGroup
	for _, j := range list {
		wg.Go(func() {
			defer j.Handle.settledCancelled()
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
