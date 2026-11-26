package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// monitorPoll is how often monitor checks a job's output.
const monitorPoll = 50 * time.Millisecond

// Tools implements agent.ToolProvider. Every tool but cancel_job only reads,
// so they run alongside other calls without approval.
func (m *Middleware) Tools() []tool.Tool {
	return []tool.Tool{
		tool.New(ToolCheck, "Show a background shell job's status, exit code and output (a job started by execute with run_in_background, not a task subagent). With wait, wait until it finishes or timeout_seconds pass.",
			m.checkJob, tool.WithReadOnly(), tool.WithConcurrent()),
		tool.New(ToolCancel, "Cancel a running background shell job and everything it started.",
			m.cancelJob, tool.WithPermission(allowCancel)),
		tool.New(ToolList, "List this conversation's background shell jobs with their status.",
			m.listJobs, tool.WithReadOnly(), tool.WithConcurrent()),
		tool.New(ToolSleep, "Wait for the given number of seconds. Returns early when a background shell job finishes, so use it instead of polling.",
			m.sleep, tool.WithReadOnly(), tool.WithConcurrent()),
		tool.New(ToolMonitor, "Wait until a background shell job's output has a line matching an RE2 pattern, the job ends, or timeout_seconds pass.",
			m.monitor, tool.WithReadOnly(), tool.WithConcurrent()),
	}
}

// allowCancel pre-approves cancel_job: it can only end a job the agent itself
// started, which approving the start already covered.
func allowCancel(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Allowed()
}

// The "capped at 300" in the descriptions below is DefaultMaxSleep in
// seconds; struct tags cannot refer to the constant.

// CheckInput is the input of check_job.
type CheckInput struct {
	// JobID is the id of the job to show.
	JobID string `json:"job_id" desc:"Id of the background shell job, such as bg_1."`
	// Wait makes the tool wait for the job to end before answering.
	Wait bool `json:"wait,omitempty" desc:"Wait for the job to finish before answering."`
	// TimeoutSeconds bounds the wait; zero or less means DefaultMaxSleep,
	// which also caps it.
	TimeoutSeconds int `json:"timeout_seconds,omitempty" desc:"Most seconds to wait when wait is set; capped at 300."`
}

// CancelInput is the input of cancel_job.
type CancelInput struct {
	// JobID is the id of the job to cancel.
	JobID string `json:"job_id" desc:"Id of the background shell job, such as bg_1."`
}

// ListInput is the input of list_jobs, which takes none.
type ListInput struct{}

// SleepInput is the input of sleep.
type SleepInput struct {
	// Seconds is how long to wait, capped at DefaultMaxSleep; it must be
	// positive.
	Seconds int `json:"seconds" desc:"Seconds to wait; capped at 300."`
	// Reason says what the model is waiting for.
	Reason string `json:"reason,omitempty" desc:"What you are waiting for."`
}

// MonitorInput is the input of monitor.
type MonitorInput struct {
	// JobID is the id of the job whose output is watched.
	JobID string `json:"job_id" desc:"Id of the background shell job, such as bg_1."`
	// Pattern is an RE2 regular expression matched against each line.
	Pattern string `json:"pattern" desc:"RE2 regular expression matched against each output line."`
	// TimeoutSeconds bounds the wait; zero or less means DefaultMaxSleep,
	// which also caps it.
	TimeoutSeconds int `json:"timeout_seconds,omitempty" desc:"Most seconds to wait; capped at 300."`
}

// waitLimit turns a requested number of seconds into a wait, capped at
// DefaultMaxSleep; zero or less means the cap.
func waitLimit(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultMaxSleep
	}
	return min(time.Duration(seconds)*time.Second, DefaultMaxSleep)
}

// lookup finds a job of the call's thread: the live job, or failing that
// the run's record of it. Records are refreshed first.
func (m *Middleware) lookup(c *tool.Call, id string) (*job, Record, bool) {
	records := m.updateRecords(c.State, c.ThreadID, nil)
	if j := m.jobs.Get(c.ThreadID, id); j != nil {
		status, exitCode := outcome(j)
		return j, Record{ID: id, ThreadID: c.ThreadID, Command: j.Handle.command, Status: status, ExitCode: exitCode, Started: j.Started}, true
	}
	for _, r := range records {
		if r.ID == id {
			return nil, r, true
		}
	}
	return nil, Record{}, false
}

func (m *Middleware) checkJob(ctx context.Context, in CheckInput, c *tool.Call) (tool.Result, error) {
	j, _, ok := m.lookup(c, in.JobID)
	if ok && j != nil && in.Wait {
		if err := waitDone(ctx, j, waitLimit(in.TimeoutSeconds)); err != nil {
			return tool.Result{}, err
		}
	}
	j, r, ok := m.lookup(c, in.JobID)
	if !ok {
		return tool.Errorf("unknown job %q", in.JobID), nil
	}
	var sb strings.Builder
	sb.WriteString(describe(r))
	if j == nil {
		sb.WriteString(recordedOutput(r))
		return tool.Text(sb.String()), nil
	}
	sb.WriteString("\n")
	if res, done := j.Handle.Result(); done {
		sb.WriteString(res.Output)
		if res.Truncated {
			sb.WriteString("\n[output truncated]")
		}
	} else {
		sb.WriteString(j.Handle.Output())
	}
	return tool.Text(sb.String()), nil
}

// recordedOutput renders the output kept in a record whose live job is gone.
func recordedOutput(r Record) string {
	if r.OutputTail == "" {
		return "\n[output not available]"
	}
	return "\n[end of output]\n" + r.OutputTail
}

// waitDone waits until j ends or limit passes; only ctx ending is an error.
func waitDone(ctx context.Context, j *job, limit time.Duration) error {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-j.Handle.Done():
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// describe renders a job's one-line summary.
func describe(r Record) string {
	if !r.finished() {
		return fmt.Sprintf("Job %s (`%s`): %s", r.ID, r.Command, r.Status)
	}
	return fmt.Sprintf("Job %s (`%s`): %s (exit code %d)", r.ID, r.Command, r.Status, r.ExitCode)
}

func (m *Middleware) cancelJob(ctx context.Context, in CancelInput, c *tool.Call) (tool.Result, error) {
	j, r, ok := m.lookup(c, in.JobID)
	switch {
	case !ok:
		return tool.Errorf("unknown job %q", in.JobID), nil
	case r.finished():
		return tool.Text(fmt.Sprintf("Job %s already ended: %s.", r.ID, r.Status)), nil
	}
	if err := stop(ctx, j); err != nil {
		return tool.Errorf("%v", err), nil
	}
	// The model asked for the cancel, so it needs no notification about it.
	m.updateRecords(c.State, c.ThreadID, func(r *Record) {
		if r.ID == in.JobID && r.finished() {
			m.markNotified(r)
		}
	})
	return tool.Text(fmt.Sprintf("Cancelled job %s.", in.JobID)), nil
}

func (m *Middleware) listJobs(_ context.Context, _ ListInput, c *tool.Call) (tool.Result, error) {
	records := m.updateRecords(c.State, c.ThreadID, nil)
	if len(records) == 0 {
		return tool.Text("No background jobs."), nil
	}
	lines := make([]string, len(records))
	for i, r := range records {
		lines[i] = describe(r)
	}
	return tool.Text(strings.Join(lines, "\n")), nil
}

func (m *Middleware) sleep(ctx context.Context, in SleepInput, c *tool.Call) (tool.Result, error) {
	if in.Seconds <= 0 {
		return tool.Errorf("seconds must be positive"), nil
	}
	limit := waitLimit(in.Seconds)
	changed, lastFinished := m.jobs.Changed(c.ThreadID)
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-changed:
		return tool.Text(fmt.Sprintf("woke: job %s finished", lastFinished())), nil
	case <-timer.C:
		return tool.Text(fmt.Sprintf("slept %s", limit)), nil
	case <-ctx.Done():
		return tool.Result{}, ctx.Err()
	}
}

func (m *Middleware) monitor(ctx context.Context, in MonitorInput, c *tool.Call) (tool.Result, error) {
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return tool.Errorf("invalid pattern: %v", err), nil
	}
	j, r, ok := m.lookup(c, in.JobID)
	switch {
	case !ok:
		return tool.Errorf("unknown job %q", in.JobID), nil
	case j == nil:
		return tool.Text(describe(r) + recordedOutput(r)), nil
	}
	limit := waitLimit(in.TimeoutSeconds)
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	poll := time.NewTicker(monitorPoll)
	defer poll.Stop()
	for {
		// The outcome is read before the output, so a job that ended is
		// never reported as ended without its final output being searched.
		status, exitCode := outcome(j)
		if line, found := matchingLine(re, j.Handle.Output()); found {
			return tool.Text(fmt.Sprintf("Pattern matched in job %s (%s):\n%s", j.ID, statusText(status, exitCode), line)), nil
		}
		if status != StatusRunning {
			return tool.Text(fmt.Sprintf("Job %s ended without a match: %s.", j.ID, statusText(status, exitCode))), nil
		}
		select {
		case <-poll.C:
		case <-j.Handle.Done():
		case <-deadline.C:
			return tool.Text(fmt.Sprintf("No match in job %s within %s; it is still running.", j.ID, limit)), nil
		case <-ctx.Done():
			return tool.Result{}, ctx.Err()
		}
	}
}

// statusText renders a status, with the exit code once the job ended.
func statusText(status Status, exitCode int) string {
	if status == StatusRunning {
		return "still running"
	}
	return fmt.Sprintf("%s, exit code %d", status, exitCode)
}

// matchingLine returns the first line of output that re matches.
func matchingLine(re *regexp.Regexp, output string) (string, bool) {
	for line := range strings.Lines(output) {
		line = strings.TrimRight(line, "\r\n")
		if re.MatchString(line) {
			return line, true
		}
	}
	return "", false
}
