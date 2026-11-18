// Package tasks runs shell commands as background jobs of a thread. With it,
// the filesystem middleware's execute tool accepts run_in_background (see
// filesystem.Config.Background), and the model gets tools to read a job's
// output (task_output), stop it (task_stop), list jobs (task_list), wait for
// one to finish (sleep) and watch its output for a pattern (monitor).
//
// When a job ends, the next turn of the thread starts with a user message
// announcing it, so the model learns of it without polling. Jobs belong to a
// thread, not to a run: a top-level run may end while its jobs keep running,
// and a later run on the thread picks them up. A nested (subagent) run's jobs
// are stopped when it ends. Shutdown stops every job; call it before the
// program exits, since jobs are processes of the host.
//
// Job records live in the run's State (JobsKey); a job recorded as running
// that the middleware does not know, because the State was saved by an
// earlier process, is reported as lost.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Defaults applied when the matching Config field is zero, and the limit on
// how long a tool waits.
const (
	// DefaultMaxRunning is how many jobs may run at once in one thread.
	DefaultMaxRunning = 8
	// DefaultJobTimeout stops a job that runs this long.
	DefaultJobTimeout = 30 * time.Minute
	// DefaultMaxFinished is how many ended jobs are remembered per thread.
	DefaultMaxFinished = 50
	// DefaultMaxSleep caps how long sleep, monitor and a blocking
	// task_output wait.
	DefaultMaxSleep = 5 * time.Minute
)

// Tool names.
const (
	ToolTaskOutput = "task_output"
	ToolTaskStop   = "task_stop"
	ToolTaskList   = "task_list"
	ToolSleep      = "sleep"
	ToolMonitor    = "monitor"
)

// NotificationMetaKey marks a job notification message: its llm.Message.Meta
// holds the job's id under this key.
const NotificationMetaKey = "orchestrix.task_notification"

// The values of JobRecord.Status.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
	StatusStopped   = "stopped"
	// StatusLost marks a job recorded as running that this process does not
	// know, typically because the State was saved before a restart.
	StatusLost = "lost"
)

// lostExitCode is the exit code recorded for a lost job, which has none.
const lostExitCode = -1

// JobRecord is a run's record of one background job.
type JobRecord struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	Command  string `json:"command"`
	// Status is one of the Status constants.
	Status string `json:"status"`
	// ExitCode is set once the job ended; -1 when it was killed or lost.
	ExitCode int       `json:"exit_code"`
	Started  time.Time `json:"started"`
	// Ended is when the middleware observed the end (a tool or the next
	// turn looked at the job), not the exact exit time; zero while the job
	// runs.
	Ended time.Time `json:"ended,omitzero"`
	// Notified reports that the model was told the job ended, by a
	// notification or because it stopped the job itself.
	Notified bool `json:"notified,omitempty"`
}

// finished reports whether the record describes an ended job.
func (r JobRecord) finished() bool { return r.Status != StatusRunning }

// JobsKey holds the run's job records, oldest first. It is private to the run
// that owns it.
var JobsKey = state.NewKey[[]JobRecord]("tasks.jobs", state.Private[[]JobRecord]())

// Config configures the tasks middleware.
type Config struct {
	// MaxRunning is how many jobs may run at once per thread; 0 means
	// DefaultMaxRunning.
	MaxRunning int
	// JobTimeout is the timeout of a job started without one, and the most a
	// job may ask for; 0 means DefaultJobTimeout.
	JobTimeout time.Duration
	// MaxFinished is how many ended jobs are remembered per thread for
	// task_output; 0 means DefaultMaxFinished.
	MaxFinished int
}

// Middleware is the tasks middleware. Unlike most middleware it holds state
// of its own, the live jobs, which it shares between every run and agent it
// is used with; that state is safe for concurrent use.
type Middleware struct {
	jobTimeout time.Duration
	jobs       *manager
}

var (
	_ agent.ToolProvider = (*Middleware)(nil)
	_ agent.ToolWrapper  = (*Middleware)(nil)
	_ agent.BeforeModel  = (*Middleware)(nil)
	_ agent.AfterAgent   = (*Middleware)(nil)
)

// New returns the tasks middleware.
func New(cfg Config) *Middleware {
	return &Middleware{
		jobTimeout: positiveOr(cfg.JobTimeout, DefaultJobTimeout),
		jobs:       newManager(positiveOr(cfg.MaxRunning, DefaultMaxRunning), positiveOr(cfg.MaxFinished, DefaultMaxFinished)),
	}
}

func positiveOr[T int | time.Duration](v, fallback T) T {
	if v > 0 {
		return v
	}
	return fallback
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return "tasks" }

// Shutdown stops every job of every thread and waits for them to end, for at
// most twice backend.DefaultKillGrace. A job still running after that keeps
// one goroutine waiting for it. Jobs cannot be started afterwards, and the
// records of the jobs it stopped show them as stopped.
func (m *Middleware) Shutdown() { m.jobs.shutdown() }

// WrapTool implements agent.ToolWrapper: it offers background execution to
// every tool call.
func (m *Middleware) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	if call.Call != nil {
		call.Call.Background = starter{m}
	}
	return next(ctx, call)
}

// starter implements tool.Background on top of the middleware's jobs. The job
// belongs to the thread of the call it is started from.
type starter struct{ m *Middleware }

// Start implements tool.Background.
func (s starter) Start(ctx context.Context, c *tool.Call, command string, timeout time.Duration) (string, error) {
	if c.ThreadID == "" {
		// Jobs are found by thread: without one, unrelated runs would share
		// them and nobody could pick up a job after its run ended.
		return "", errors.New("background jobs need a thread id")
	}
	b, ok := c.Backend.(backend.Starter)
	if !ok {
		return "", errors.New("this backend cannot run background commands")
	}
	if c.State == nil {
		return "", errors.New("background commands need the run's state")
	}
	if timeout <= 0 || timeout > s.m.jobTimeout {
		timeout = s.m.jobTimeout
	}
	recorded, _ := JobsKey.Get(c.State)
	j, err := s.m.jobs.start(ctx, b, c.ThreadID, command, timeout, recorded)
	if err != nil {
		return "", err
	}
	JobsKey.Update(c.State, func(records []JobRecord) []JobRecord {
		return append(slices.Clone(records), JobRecord{
			ID: j.id, ThreadID: j.threadID, Command: command, Status: StatusRunning, Started: j.started,
		})
	})
	return j.id, nil
}

// BeforeModel implements agent.BeforeModel: every job that ended since the
// last turn, or was lost, is announced with one user message.
//
// Before that, a thread whose jobs all ended and were announced in earlier
// turns is released (see manager.release): the model had the turn of each
// notification to read the output, and afterwards task_output reports the
// recorded outcome only.
func (m *Middleware) BeforeModel(ctx context.Context, s *state.State) error {
	if info, ok := agent.Info(ctx); ok && info.ThreadID != "" {
		m.jobs.release(info.ThreadID)
	}
	var notices []llm.Message
	m.updateRecords(s, func(records []JobRecord) {
		for i := range records {
			if records[i].finished() && !records[i].Notified {
				notices = append(notices, notification(records[i]))
				m.markNotified(&records[i])
			}
		}
	})
	s.AppendMessages(notices...)
	return nil
}

// markNotified records, in r and on its job, that the model was told the job
// ended.
func (m *Middleware) markNotified(r *JobRecord) {
	r.Notified = true
	m.jobs.markNotified(r.ThreadID, []string{r.ID})
}

func notification(r JobRecord) llm.Message {
	msg := llm.UserText(fmt.Sprintf("<task-notification>Background task %s (`%s`) %s (exit code %d). Use %s to read its output.</task-notification>",
		r.ID, r.Command, r.Status, r.ExitCode, ToolTaskOutput))
	msg.Meta = map[string]string{NotificationMetaKey: r.ID}
	return msg
}

// AfterAgent implements agent.AfterAgent: a nested run's jobs are stopped and
// forgotten when it ends, so a subagent's jobs never outlive it. A top-level
// run's jobs keep running for later runs of the thread.
func (m *Middleware) AfterAgent(ctx context.Context, s *state.State) error {
	if info, ok := agent.Info(ctx); !ok || info.Depth == 0 {
		return nil
	}
	records, _ := JobsKey.Get(s)
	byThread := map[string][]string{}
	for _, r := range records {
		if !r.finished() {
			byThread[r.ThreadID] = append(byThread[r.ThreadID], r.ID)
		}
	}
	for threadID, ids := range byThread {
		m.jobs.stopAndForget(threadID, ids, func() {
			m.updateRecords(s, func(records []JobRecord) {
				for i := range records {
					if slices.Contains(ids, records[i].ID) {
						m.markNotified(&records[i])
					}
				}
			})
		})
	}
	return nil
}

// updateRecords brings the run's records up to date with the jobs, then lets
// edit change them, in one State update, and returns a copy of the result.
// A running record whose job ended takes its outcome; one whose job the
// middleware does not know is lost. edit may be nil.
func (m *Middleware) updateRecords(s *state.State, edit func([]JobRecord)) []JobRecord {
	var out []JobRecord
	JobsKey.Update(s, func(records []JobRecord) []JobRecord {
		records = slices.Clone(records)
		for i := range records {
			m.refresh(&records[i])
		}
		if edit != nil {
			edit(records)
		}
		records = pruneRecords(records, m.jobs.maxFinished)
		out = slices.Clone(records)
		return records
	})
	return out
}

// refresh updates a running record from its job.
func (m *Middleware) refresh(r *JobRecord) {
	if r.finished() {
		return
	}
	j := m.jobs.job(r.ThreadID, r.ID)
	if j == nil {
		r.Status, r.ExitCode, r.Ended = StatusLost, lostExitCode, time.Now()
		return
	}
	if status, exitCode := j.outcome(); status != StatusRunning {
		r.Status, r.ExitCode, r.Ended = status, exitCode, time.Now()
	}
}

// pruneRecords drops the oldest records the model has already been told
// about, keeping at most keep of them, so the records stay bounded.
func pruneRecords(records []JobRecord, keep int) []JobRecord {
	settled := 0
	for _, r := range records {
		if r.finished() && r.Notified {
			settled++
		}
	}
	excess := settled - keep
	return slices.DeleteFunc(records, func(r JobRecord) bool {
		if excess > 0 && r.finished() && r.Notified {
			excess--
			return true
		}
		return false
	})
}
