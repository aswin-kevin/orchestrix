package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	registry "github.com/aswin-kevin/orchestrix/internal/jobs"
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
	// DefaultMaxSleep caps how long sleep, monitor and a waiting
	// check_job wait.
	DefaultMaxSleep = 5 * time.Minute
)

// Name is the middleware's name, as Middleware.Name returns it.
const Name = "jobs"

// Tool names.
const (
	// ToolCheck shows a job's status and output, waiting for its end when
	// asked to.
	ToolCheck = "check_job"
	// ToolCancel cancels a running job.
	ToolCancel = "cancel_job"
	// ToolList lists the thread's jobs.
	ToolList = "list_jobs"
	// ToolSleep waits, returning early when a job of the thread ends.
	ToolSleep = "sleep"
	// ToolMonitor waits for a line of a job's output to match a pattern.
	ToolMonitor = "monitor"
)

// NotificationMetaKey marks a job notification message: its llm.Message.Meta
// holds the job's id under this key, and Name under agent.InjectedMetaKey.
const NotificationMetaKey = "orchestrix.job_notification"

// Status is the state of a job, as a Record holds it.
type Status string

// The values of Record.Status.
const (
	// StatusRunning marks a job that has not ended.
	StatusRunning Status = "running"
	// StatusCompleted marks a job that exited with code 0.
	StatusCompleted Status = "completed"
	// StatusFailed marks a job that exited with another code.
	StatusFailed Status = "failed"
	// StatusTimedOut marks a job stopped because it outran its timeout.
	StatusTimedOut Status = "timed_out"
	// StatusCancelled marks a job stopped on purpose: by cancel_job, by
	// Close, or because the nested run that started it ended.
	StatusCancelled Status = "cancelled"
	// StatusLost marks a job recorded as running that this process does not
	// know, typically because the State was saved before a restart.
	StatusLost Status = "lost"
)

// lostExitCode is the exit code recorded for a lost job, which has none.
const lostExitCode = -1

// Record is a run's record of one background job.
type Record struct {
	// ID is the job's id, the one the model passes to check_job and
	// cancel_job.
	ID string `json:"id"`
	// ThreadID is the thread whose run started the job.
	ThreadID string `json:"thread_id"`
	// Command is the shell command line the job runs.
	Command string `json:"command"`
	// Status is one of the Status constants.
	Status Status `json:"status"`
	// ExitCode is set once the job ended; -1 when it was killed or lost.
	ExitCode int `json:"exit_code"`
	// Started is when the job was started.
	Started time.Time `json:"started"`
	// Ended is when the middleware observed the end (a tool or the next
	// turn looked at the job), not the exact exit time; zero while the job
	// runs.
	Ended time.Time `json:"ended,omitzero"`
	// Notified reports that the model was told the job ended, by a
	// notification or because it stopped the job itself.
	Notified bool `json:"notified,omitempty"`
	// OutputTail is the end of the job's output, at most outputTailBytes,
	// kept when its end is observed so check_job can still show it after
	// the live job was released or evicted; empty for a lost job.
	OutputTail string `json:"output_tail,omitempty"`
}

// outputTailBytes caps Record.OutputTail; records live in State, which
// is saved with every checkpoint, so they keep only a short tail.
const outputTailBytes = 4 << 10

// outputTail returns the last outputTailBytes of output at most, starting
// on a rune boundary.
func outputTail(output string) string {
	if len(output) <= outputTailBytes {
		return output
	}
	tail := output[len(output)-outputTailBytes:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail
}

// finished reports whether the record describes an ended job.
func (r Record) finished() bool { return r.Status != StatusRunning }

// recordAccessors lets the registry's record helpers read a Record: a record
// is settled, and so may be pruned, once the job ended and the model was told
// so.
var recordAccessors = registry.Accessors[Record]{
	Thread:  func(r Record) string { return r.ThreadID },
	Settled: func(r Record) bool { return r.finished() && r.Notified },
}

// recordsKey holds the run's job records, oldest first. It is run-local
// (state.RunLocal): neither a subagent nor a fork inherits the records, since
// the jobs belong to the thread that started them.
var recordsKey = state.NewKey[[]Record]("jobs.records", state.RunLocal[[]Record]())

// Records returns a copy of the job records in s, oldest first, of every
// thread whose runs used s. It returns nil when s holds none.
func Records(s *state.State) []Record {
	records, _ := recordsKey.Get(s)
	return slices.Clone(records)
}

// Config configures the jobs middleware. A zero field means its default; a
// negative one makes New fail.
type Config struct {
	// MaxRunning is how many jobs may run at once per thread; 0 means
	// DefaultMaxRunning.
	MaxRunning int
	// JobTimeout is the timeout of a job started without one, and the most a
	// job may ask for; 0 means DefaultJobTimeout.
	JobTimeout time.Duration
	// MaxFinished is how many ended jobs are remembered per thread for
	// check_job; 0 means DefaultMaxFinished.
	MaxFinished int
}

// Middleware is the jobs middleware. Unlike most middleware it holds state
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
	_ agent.Closer       = (*Middleware)(nil)
)

// New returns the jobs middleware. It fails when a Config field is
// negative.
func New(cfg Config) (*Middleware, error) {
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"MaxRunning", int64(cfg.MaxRunning)},
		{"JobTimeout", int64(cfg.JobTimeout)},
		{"MaxFinished", int64(cfg.MaxFinished)},
	} {
		if f.v < 0 {
			return nil, fmt.Errorf("jobs: %s must not be negative", f.name)
		}
	}
	return &Middleware{
		jobTimeout: positiveOr(cfg.JobTimeout, DefaultJobTimeout),
		jobs:       newManager(positiveOr(cfg.MaxRunning, DefaultMaxRunning), positiveOr(cfg.MaxFinished, DefaultMaxFinished)),
	}, nil
}

func positiveOr[T int | time.Duration](v, fallback T) T {
	if v > 0 {
		return v
	}
	return fallback
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Close implements agent.Closer, so Agent.Close stops the jobs: it stops
// every job of every thread and waits for them to end, for at most twice
// backend.DefaultKillGrace, or until ctx's deadline when that comes sooner;
// the stop requests, sent before that wait, may add up to one more
// backend.DefaultKillGrace.
// A job still running after that keeps one goroutine waiting for it. Jobs
// cannot be started afterwards, and the records of the jobs it stopped show
// them as cancelled. It returns an error when a job had not ended by then
// (ctx's error when ctx ended). Calling it more than once is safe.
func (m *Middleware) Close(ctx context.Context) error { return m.jobs.Close(ctx, stopWait) }

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
	b, ok := backend.AsStarter(c.Backend)
	if !ok {
		return "", errors.New("this backend cannot run background commands")
	}
	if c.State == nil {
		return "", errors.New("background commands need the run's state")
	}
	if timeout <= 0 || timeout > s.m.jobTimeout {
		timeout = s.m.jobTimeout
	}
	recorded, _ := recordsKey.Get(c.State)
	j, err := startJob(ctx, s.m.jobs, b, c.ThreadID, command, timeout, recorded)
	if err != nil {
		return "", err
	}
	recordsKey.Update(c.State, func(records []Record) []Record {
		return append(slices.Clone(records), Record{
			ID: j.ID, ThreadID: j.ThreadID, Command: command, Status: StatusRunning, Started: j.Started,
		})
	})
	return j.ID, nil
}

// BeforeModel implements agent.BeforeModel: every job of the run's thread
// that ended since the last turn, or was lost, is announced with one user
// message. Records of other threads are left alone, so a run never takes
// another thread's notifications.
//
// Before that, a thread whose jobs all ended and were announced in earlier
// turns is released (see registry.Manager.Release): the model had the turn
// of each notification to read the output, and afterwards check_job reports
// the recorded outcome only.
func (m *Middleware) BeforeModel(ctx context.Context, s *state.State) error {
	threadID, _ := agent.ThreadID(ctx)
	if threadID != "" {
		m.jobs.Release(threadID)
	}
	var notices []llm.Message
	m.updateRecords(s, threadID, func(r *Record) {
		if r.finished() && !r.Notified {
			notices = append(notices, notification(*r))
			m.markNotified(r)
		}
	})
	s.AppendMessages(notices...)
	return nil
}

// markNotified records, in r and on its job, that the model was told the job
// ended.
func (m *Middleware) markNotified(r *Record) {
	r.Notified = true
	m.jobs.MarkNotified(r.ThreadID, r.ID)
}

func notification(r Record) llm.Message {
	msg := llm.UserText(fmt.Sprintf("<job-notification>Background job %s (`%s`) %s (exit code %d). Use %s to read its output.</job-notification>",
		r.ID, r.Command, r.Status, r.ExitCode, ToolCheck))
	msg.Meta = map[string]string{NotificationMetaKey: r.ID, agent.InjectedMetaKey: Name}
	return msg
}

// AfterAgent implements agent.AfterAgent: a nested run's jobs are stopped and
// forgotten when it ends, so a subagent's jobs never outlive it. A top-level
// run's jobs keep running for later runs of the thread. Only jobs of the
// run's own thread are stopped: a State may carry records of other threads,
// and those jobs belong to whoever runs them.
func (m *Middleware) AfterAgent(ctx context.Context, s *state.State) error {
	info, ok := agent.Info(ctx)
	if !ok || info.Depth == 0 {
		return nil
	}
	var ids []string
	for _, r := range m.threadRecords(s, info.ThreadID) {
		if !r.finished() {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	m.jobs.StopAndForget(ctx, info.ThreadID, ids, stopWait, func() {
		m.updateRecords(s, info.ThreadID, func(r *Record) {
			if slices.Contains(ids, r.ID) {
				m.markNotified(r)
			}
		})
	})
	return nil
}

// threadRecords returns the run's records of threadID, as stored.
func (m *Middleware) threadRecords(s *state.State, threadID string) []Record {
	records, _ := recordsKey.Get(s)
	return registry.ThreadRecords(records, threadID, recordAccessors)
}

// updateRecords brings the run's records of threadID up to date with the
// jobs, then lets edit change each of them, in one State update, and returns
// a copy of the records of threadID. A running record whose job ended takes
// its outcome; one whose job the middleware does not know is lost. Records
// of other threads are kept untouched. edit may be nil.
func (m *Middleware) updateRecords(s *state.State, threadID string, edit func(*Record)) []Record {
	return registry.UpdateRecords(s, recordsKey, threadID, m.jobs.MaxFinished(), recordAccessors, func(r *Record) {
		m.refresh(r)
		if edit != nil {
			edit(r)
		}
	})
}

// refresh updates a running record from its job.
func (m *Middleware) refresh(r *Record) {
	if r.finished() {
		return
	}
	j := m.jobs.Get(r.ThreadID, r.ID)
	if j == nil {
		r.Status, r.ExitCode, r.Ended = StatusLost, lostExitCode, time.Now()
		return
	}
	if status, exitCode := outcome(j); status != StatusRunning {
		r.Status, r.ExitCode, r.Ended = status, exitCode, time.Now()
		res, _ := j.Handle.Result()
		r.OutputTail = outputTail(res.Output)
	}
}
