// Package async runs subagents in the background. With it, the model starts a
// subagent with start_task and keeps working while it runs; it can look at a
// task's progress or wait for it (check_task), send it new instructions
// (update_task), cancel it (cancel_task) and list the tasks of the
// conversation (list_tasks). When a task ends, the next
// turn of the thread starts with a user message announcing it with an
// excerpt of its result, so the model learns of it without polling.
//
// A background subagent is started exactly like one of the task tool (see
// subagents.Launcher): the same specs, the stricter permission mode, the
// narrowed skills scope and the call's backend. By default it runs in this
// process; Config.Runner runs tasks elsewhere instead.
//
// A background run has nobody to ask for approval in-band: with an Approver
// on the parent agent the subagent inherits it, otherwise a subagent that
// needs a decision ends as StatusBlocked, naming the tools it wanted to
// call. A background subagent's State is not merged into the parent's (the
// parent may have moved on by the time it ends); files are shared through
// the backend.
//
// Tasks belong to a thread, not to a run: a top-level run may end while its
// tasks keep running, and a later run on the thread is told when they end.
// A run about to end while its tasks run is reminded of them once
// (Config.DisableStopReminder). The reminder is a BeforeStop continuation:
// it uses one of the run's agent.MaxStopContinuations slots, shared with
// every other BeforeStop middleware (rubric, hooks Stop hooks), and, placed
// after them in the stack, it is consulted before them. A nested run's
// tasks are stopped when it ends, so a subagent's own tasks never outlive
// it. Shutdown stops every task; call it before the program exits.
//
// Task records live in the run's State (TasksKey); a run only reads,
// announces and cancels the tasks of its own thread. While tasks run, the
// system prompt lists them from those records, so the model still knows of
// them after a compaction. Tasks are not re-attached in a new process: a
// record of a running task this process does not know is reported to the
// model as lost.
package async

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/jobs"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
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
	// DefaultStopWait bounds how long cancel_task and Shutdown wait for
	// stopped tasks to end.
	DefaultStopWait = 5 * time.Second
	// DefaultMaxResultChars caps, in bytes, the result excerpt shown in a
	// notification and by list_tasks.
	DefaultMaxResultChars = 4_000
	// MaxRecordedResult caps, in bytes, TaskRecord.Result and
	// TaskRecord.Error: records live in State, which every checkpoint saves.
	MaxRecordedResult = 16 << 10
	// DefaultProgressChars is how much of its most recent text, in bytes, a
	// running task of the local runner reports as progress.
	DefaultProgressChars = 2_000
	// IDPrefix starts every task id; the rest is the task's number in its
	// thread.
	IDPrefix = "async_"
	// NotificationMetaKey marks a task notification message: its
	// llm.Message.Meta holds the task's id under this key, and "async" under
	// agent.InjectedMetaKey.
	NotificationMetaKey = "orchestrix.async_notification"
)

// descriptionPreviewChars caps, in bytes, the task description list_tasks
// shows.
const descriptionPreviewChars = 120

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
	// Runner starts the tasks; nil runs them as subagents in this process.
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
}

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
		specs:    make(map[string]subagents.Subagent, len(list)),
		order:    make([]string, len(list)),
		runner:   cfg.Runner,
		maxDepth: positiveOr(cfg.MaxDepth, subagents.DefaultMaxDepth),
		prompt:   DefaultPrompt,
		remind:   !cfg.DisableStopReminder,
		jobs: jobs.NewManager[*task](IDPrefix, jobs.Limits{
			MaxRunning:  positiveOr(cfg.MaxRunning, DefaultMaxRunning),
			MaxFinished: positiveOr(cfg.MaxFinished, DefaultMaxFinished),
		}),
	}
	for i, s := range list {
		m.specs[s.Name], m.order[i] = s, s.Name
	}
	if m.runner == nil {
		// The inbox middleware is one pointer: every child, and their
		// children, share it, and the Launcher dedupes it by identity.
		boxes := newInboxes()
		m.runner = &localRunner{specs: m.specs, launcher: subagents.NewLauncher(boxes), inboxes: boxes, timeout: positiveOr(cfg.TaskTimeout, DefaultTaskTimeout)}
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
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
func (m *Middleware) Name() string { return "async" }

// Shutdown stops every task of every thread and waits up to DefaultStopWait
// for them to end. Tasks cannot be started afterwards, and the records of the
// tasks it stopped show them as cancelled (as the runner reports them).
func (m *Middleware) Shutdown() { m.jobs.Shutdown(DefaultStopWait) }

// BeforeModel implements agent.BeforeModel: every task of the run's thread
// that ended since the last turn is announced with one user message (see
// notification). Records of other threads are left alone, so a run never
// takes another thread's notifications.
//
// Before that, a thread whose tasks all ended and were announced in earlier
// turns is released (see jobs.Manager.Release); check_task then reports the
// recorded outcome.
func (m *Middleware) BeforeModel(ctx context.Context, s *state.State) error {
	threadID := runThread(ctx)
	if threadID != "" {
		m.jobs.Release(threadID)
	}
	var notices []llm.Message
	m.updateRecords(s, threadID, func(r *TaskRecord) {
		if r.finished() && !r.Notified {
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
		records, _ := TasksKey.Get(req.State)
		if list := runningList(jobs.ThreadRecords(records, runThread(ctx)), time.Now()); list != "" {
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
	ids := runningIDs(m.updateRecords(s, runThread(ctx), nil))
	if len(ids) == 0 {
		return "", nil
	}
	remindedKey.Set(s, true)
	return fmt.Sprintf(stopReminder, strings.Join(ids, ", ")), nil
}

// AfterAgent implements agent.AfterAgent: a nested run's tasks are stopped
// and forgotten when it ends, so a subagent's background tasks never outlive
// it; their records end cancelled, and as seen, since nobody is left to be
// told. A top-level run's tasks keep running for later runs of the thread.
// Only tasks of the run's own thread are stopped.
func (m *Middleware) AfterAgent(ctx context.Context, s *state.State) error {
	info, ok := agent.Info(ctx)
	if !ok || info.Depth == 0 {
		return nil
	}
	records, _ := TasksKey.Get(s)
	ids := runningIDs(jobs.ThreadRecords(records, info.ThreadID))
	if len(ids) == 0 {
		return nil
	}
	m.jobs.StopAndForget(info.ThreadID, ids, DefaultStopWait, func() {
		m.updateRecords(s, info.ThreadID, func(r *TaskRecord) {
			if slices.Contains(ids, r.ID) {
				m.markNotified(r)
			}
		})
	})
	return nil
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

// runThread is the thread of the run ctx belongs to; empty outside a run.
func runThread(ctx context.Context) string {
	info, _ := agent.Info(ctx)
	return info.ThreadID
}

// markNotified records, in r and on its job, that the model was told the
// task ended.
func (m *Middleware) markNotified(r *TaskRecord) {
	r.Notified = true
	m.jobs.MarkNotified(r.ThreadID, r.ID)
}
