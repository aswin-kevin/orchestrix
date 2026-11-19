package tasks

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

// Tools implements agent.ToolProvider. Every tool but task_stop only reads,
// so they run alongside other calls without approval.
func (m *Middleware) Tools() []tool.Tool {
	return []tool.Tool{
		tool.New(ToolTaskOutput, "Show a background shell job's status, exit code and output (a job started by execute with run_in_background, not a task subagent). With block, wait until it finishes or timeout_seconds pass.",
			m.taskOutput, tool.WithReadOnly(), tool.WithConcurrent()),
		tool.New(ToolTaskStop, "Stop a running background shell job and everything it started.",
			m.taskStop, tool.WithPermission(allowStop)),
		tool.New(ToolTaskList, "List this conversation's background shell jobs with their status.",
			m.taskList, tool.WithReadOnly(), tool.WithConcurrent()),
		tool.New(ToolSleep, "Wait for the given number of seconds. Returns early when a background shell job finishes, so use it instead of polling.",
			m.sleep, tool.WithReadOnly(), tool.WithConcurrent()),
		tool.New(ToolMonitor, "Wait until a background shell job's output has a line matching an RE2 pattern, the job ends, or timeout_seconds pass.",
			m.monitor, tool.WithReadOnly(), tool.WithConcurrent()),
	}
}

// allowStop pre-approves task_stop: it can only end a job the agent itself
// started, which approving the start already covered.
func allowStop(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Allowed()
}

// The "capped at 300" in the descriptions below is DefaultMaxSleep in
// seconds; struct tags cannot refer to the constant.

type taskOutputInput struct {
	TaskID         string `json:"task_id" desc:"Id of the background shell job, such as bg_1."`
	Block          bool   `json:"block,omitempty" desc:"Wait for the task to finish before answering."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" desc:"Most seconds to wait when block is set; capped at 300."`
}

type taskIDInput struct {
	TaskID string `json:"task_id" desc:"Id of the background shell job, such as bg_1."`
}

type taskListInput struct{}

type sleepInput struct {
	Seconds int    `json:"seconds" desc:"Seconds to wait; capped at 300."`
	Reason  string `json:"reason,omitempty" desc:"What you are waiting for."`
}

type monitorInput struct {
	TaskID         string `json:"task_id" desc:"Id of the background shell job, such as bg_1."`
	Pattern        string `json:"pattern" desc:"RE2 regular expression matched against each output line."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" desc:"Most seconds to wait; capped at 300."`
}

// waitLimit turns a requested number of seconds into a wait, capped at
// DefaultMaxSleep; zero or less means the cap.
func waitLimit(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultMaxSleep
	}
	return min(time.Duration(seconds)*time.Second, DefaultMaxSleep)
}

// lookup finds a task of the call's thread: the live job, or failing that
// the run's record of it. Records are refreshed first.
func (m *Middleware) lookup(c *tool.Call, id string) (*job, JobRecord, bool) {
	records := m.updateRecords(c.State, c.ThreadID, nil)
	if j := m.jobs.Get(c.ThreadID, id); j != nil {
		status, exitCode := outcome(j)
		return j, JobRecord{ID: id, ThreadID: c.ThreadID, Command: j.Handle.command, Status: status, ExitCode: exitCode, Started: j.Started}, true
	}
	for _, r := range records {
		if r.ID == id {
			return nil, r, true
		}
	}
	return nil, JobRecord{}, false
}

func (m *Middleware) taskOutput(ctx context.Context, in taskOutputInput, c *tool.Call) (tool.Result, error) {
	j, _, ok := m.lookup(c, in.TaskID)
	if ok && j != nil && in.Block {
		if err := waitDone(ctx, j, waitLimit(in.TimeoutSeconds)); err != nil {
			return tool.Result{}, err
		}
	}
	j, r, ok := m.lookup(c, in.TaskID)
	if !ok {
		return tool.Errorf("unknown task %q", in.TaskID), nil
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
func recordedOutput(r JobRecord) string {
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

// describe renders a task's one-line summary.
func describe(r JobRecord) string {
	if !r.finished() {
		return fmt.Sprintf("Task %s (`%s`): %s", r.ID, r.Command, r.Status)
	}
	return fmt.Sprintf("Task %s (`%s`): %s (exit code %d)", r.ID, r.Command, r.Status, r.ExitCode)
}

func (m *Middleware) taskStop(ctx context.Context, in taskIDInput, c *tool.Call) (tool.Result, error) {
	j, r, ok := m.lookup(c, in.TaskID)
	switch {
	case !ok:
		return tool.Errorf("unknown task %q", in.TaskID), nil
	case r.finished():
		return tool.Text(fmt.Sprintf("Task %s already ended: %s.", r.ID, r.Status)), nil
	}
	if err := stop(ctx, j); err != nil {
		return tool.Errorf("%v", err), nil
	}
	// The model asked for the stop, so it needs no notification about it.
	m.updateRecords(c.State, c.ThreadID, func(r *JobRecord) {
		if r.ID == in.TaskID && r.finished() {
			m.markNotified(r)
		}
	})
	return tool.Text(fmt.Sprintf("Stopped task %s.", in.TaskID)), nil
}

func (m *Middleware) taskList(_ context.Context, _ taskListInput, c *tool.Call) (tool.Result, error) {
	records := m.updateRecords(c.State, c.ThreadID, nil)
	if len(records) == 0 {
		return tool.Text("No background tasks."), nil
	}
	lines := make([]string, len(records))
	for i, r := range records {
		lines[i] = describe(r)
	}
	return tool.Text(strings.Join(lines, "\n")), nil
}

func (m *Middleware) sleep(ctx context.Context, in sleepInput, c *tool.Call) (tool.Result, error) {
	if in.Seconds <= 0 {
		return tool.Errorf("seconds must be positive"), nil
	}
	limit := waitLimit(in.Seconds)
	changed, lastFinished := m.jobs.Changed(c.ThreadID)
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-changed:
		return tool.Text(fmt.Sprintf("woke: task %s finished", lastFinished())), nil
	case <-timer.C:
		return tool.Text(fmt.Sprintf("slept %s", limit)), nil
	case <-ctx.Done():
		return tool.Result{}, ctx.Err()
	}
}

func (m *Middleware) monitor(ctx context.Context, in monitorInput, c *tool.Call) (tool.Result, error) {
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return tool.Errorf("invalid pattern: %v", err), nil
	}
	j, r, ok := m.lookup(c, in.TaskID)
	switch {
	case !ok:
		return tool.Errorf("unknown task %q", in.TaskID), nil
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
			return tool.Text(fmt.Sprintf("Pattern matched in task %s (%s):\n%s", j.ID, statusText(status, exitCode), line)), nil
		}
		if status != StatusRunning {
			return tool.Text(fmt.Sprintf("Task %s ended without a match: %s.", j.ID, statusText(status, exitCode))), nil
		}
		select {
		case <-poll.C:
		case <-j.Handle.Done():
		case <-deadline.C:
			return tool.Text(fmt.Sprintf("No match in task %s within %s; it is still running.", j.ID, limit)), nil
		case <-ctx.Done():
			return tool.Result{}, ctx.Err()
		}
	}
}

// statusText renders a status, with the exit code once the job ended.
func statusText(status string, exitCode int) string {
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
