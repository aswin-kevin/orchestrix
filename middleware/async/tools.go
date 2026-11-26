package async

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/jobs"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Tools implements agent.ToolProvider. Every tool is read-only: starting a
// task changes nothing by itself (each tool the subagent calls is checked on
// its own), and the others only look at or end tasks the agent started.
func (m *Middleware) Tools() []tool.Tool { return m.tools }

// The "capped at 300" in the descriptions below is DefaultMaxWait in
// seconds; struct tags cannot refer to the constant.

// StartInput is the input of start_task.
type StartInput struct {
	// Description is the whole task; the subagent sees nothing else of the
	// conversation.
	Description string `json:"description" desc:"The complete task, with every detail the subagent needs; it does not see this conversation."`
	// SubagentType is the subagent to start, one of those the middleware
	// offers.
	SubagentType string `json:"subagent_type" desc:"Which subagent to use."`
}

// CheckInput is the input of check_task.
type CheckInput struct {
	// TaskID is the id of the task to show.
	TaskID string `json:"task_id" desc:"Id of the background task, such as async_1."`
	// Wait makes the tool wait for a running task to stop before answering.
	Wait bool `json:"wait,omitempty" desc:"Wait for the task to finish before answering."`
	// TimeoutSeconds bounds the wait; zero or less means DefaultMaxWait,
	// which also caps it.
	TimeoutSeconds int `json:"timeout_seconds,omitempty" desc:"Most seconds to wait when wait is set; capped at 300."`
}

// CancelInput is the input of cancel_task.
type CancelInput struct {
	// TaskID is the id of the task to cancel.
	TaskID string `json:"task_id" desc:"Id of the background task, such as async_1."`
}

// ListInput is the input of list_tasks, which takes none.
type ListInput struct{}

func (m *Middleware) buildTools() []tool.Tool {
	return []tool.Tool{
		tool.Build(tool.Spec{
			Name:        ToolStart,
			Description: m.describeStart(),
			Schema:      startSchema(m.order),
			Run:         m.startTask,
			Concurrent:  func(json.RawMessage) bool { return true },
			ReadOnly:    func(json.RawMessage) bool { return true },
			Permission: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
				return permission.Allowed()
			},
		}),
		tool.New(ToolCheck, checkDescription, m.checkTask, tool.WithReadOnly(), tool.WithConcurrent()),
		tool.New(ToolUpdate, fmt.Sprintf(updateDescription, MaxUpdateBytes), m.updateTask, tool.WithReadOnly()),
		tool.New(ToolCancel, cancelDescription, m.cancelTask, tool.WithReadOnly()),
		tool.New(ToolList, listDescription, m.listTasks, tool.WithReadOnly(), tool.WithConcurrent()),
	}
}

// startSchema is StartInput's schema with subagent_type limited to the
// offered names, which are only known at New.
func startSchema(order []string) map[string]any {
	schema := tool.SchemaFor[StartInput]()
	props := maps.Clone(schema["properties"].(map[string]any))
	typ := maps.Clone(props["subagent_type"].(map[string]any))
	enum := make([]any, len(order)) // tool.Validate reads enum as []any
	for i, name := range order {
		enum[i] = name
	}
	typ["enum"] = enum
	props["subagent_type"] = typ
	schema["properties"] = props
	return schema
}

// describeStart lists every subagent with its description after the tool's
// own description.
func (m *Middleware) describeStart() string {
	var b strings.Builder
	b.WriteString(startDescription)
	for _, name := range m.order {
		fmt.Fprintf(&b, "\n- %s: %s", name, m.specs[name].Description)
	}
	return b.String()
}

func (m *Middleware) startTask(ctx context.Context, raw json.RawMessage, c *tool.Call) (tool.Result, error) {
	var in StartInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tool.Errorf("invalid input: %v", err), nil
	}
	if _, ok := m.specs[in.SubagentType]; !ok {
		return tool.Errorf("unknown subagent type %q", in.SubagentType), nil
	}
	if strings.TrimSpace(in.Description) == "" {
		return tool.Errorf("the description must not be empty"), nil
	}
	if c.ThreadID == "" {
		// Tasks are found by thread: without one, unrelated runs would share
		// them and nobody could be told when one ends.
		return tool.Errorf("background tasks need a thread id"), nil
	}
	info, ok := agent.Info(ctx)
	if !ok || c.State == nil {
		return tool.Errorf("background tasks can only be started inside an agent run"), nil
	}
	if info.Depth >= m.maxDepth {
		return tool.Errorf("cannot start background task %s: the maximum nesting depth of %d is reached; do the task yourself", in.SubagentType, m.maxDepth), nil
	}
	t := newTask(ctx)
	j, err := m.jobs.Start(c.ThreadID, recordedIDs(c.State), func() (*task, error) { return t, nil })
	var limit *jobs.LimitError
	switch {
	case errors.Is(err, jobs.ErrShutdown):
		return tool.Errorf("background tasks are shut down"), nil
	case errors.As(err, &limit):
		return tool.Errorf("too many background tasks running (limit %d); wait for one to finish or cancel one with %s", limit.Limit, ToolCancel), nil
	case err != nil:
		return tool.Errorf("cannot start background task: %v", err), nil
	}
	taskThread := c.ThreadID + "/" + j.ID
	h, err := m.runner.Start(ctx, Request{TaskID: j.ID, ThreadID: c.ThreadID, TaskThreadID: taskThread, SubagentType: in.SubagentType, Description: in.Description, Call: c})
	if err == nil && h == nil {
		err = errNoHandle
	}
	if err != nil {
		// No record: the model learns of the failure from this result.
		t.fail(err)
		m.jobs.MarkNotified(c.ThreadID, j.ID)
		return tool.Errorf("cannot start background task %s: %v", in.SubagentType, err), nil
	}
	t.attach(h)
	if j.Stopped() {
		// Close came while the Runner started the task, and attach
		// stopped it; as for a failed start, this result is all there is.
		m.jobs.MarkNotified(c.ThreadID, j.ID)
		return tool.Errorf("background tasks are shut down; task %s was stopped as it started", j.ID), nil
	}
	addRecord(c.State, TaskRecord{
		ID: j.ID, ThreadID: c.ThreadID, TaskThreadID: taskThread, SubagentType: in.SubagentType, Description: in.Description,
		Status: StatusRunning, Started: j.Started,
	})
	return tool.Text(fmt.Sprintf("Started background task %s (%s). You will be notified when it finishes; use %s to see its progress.",
		j.ID, in.SubagentType, ToolCheck)), nil
}

// waitLimit turns a requested number of seconds into a wait, capped at
// DefaultMaxWait; zero or less means the cap.
func waitLimit(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultMaxWait
	}
	return min(time.Duration(seconds)*time.Second, DefaultMaxWait)
}

// checkTask reports a task's status with its progress, what it waits for,
// or, once it ended, its full result. Showing the result counts as telling
// the model the task ended, so no notification repeats it.
func (m *Middleware) checkTask(ctx context.Context, in CheckInput, c *tool.Call) (tool.Result, error) {
	j, r, ok := m.lookup(c, in.TaskID)
	if !ok {
		return tool.Errorf("unknown task %q", in.TaskID), nil
	}
	if in.Wait && j != nil && r.Status == StatusRunning {
		timer := time.NewTimer(waitLimit(in.TimeoutSeconds))
		defer timer.Stop()
		select {
		case <-j.Handle.Done():
		case <-timer.C:
		case <-ctx.Done():
			return tool.Result{}, ctx.Err()
		}
		j, r, _ = m.lookup(c, in.TaskID)
	}
	now := time.Now()
	if r.Status == StatusWaiting {
		return tool.Text(summary(r, now)), nil
	}
	if !r.terminal() {
		progress := ""
		if j != nil {
			progress = j.Handle.progress()
		}
		return tool.Text(summary(r, now) + "\n" + progressText(progress)), nil
	}
	m.markSeen(c, r.ID)
	text := r.Result
	if j != nil {
		if out, _, ok := j.Handle.outcome(); ok {
			text = out.Text // the full answer; the record keeps a capped one
		}
	}
	if text == "" {
		return tool.Text(summary(r, now)), nil
	}
	text, cut := promptsafe.CapBytes(text, MaxCheckResultBytes)
	body := summary(r, now) + "\n" + frame(tagResult, neutralize(text))
	if cut {
		body += fmt.Sprintf("\n(The result was truncated to its first %d bytes.)", MaxCheckResultBytes)
	}
	return tool.Text(body), nil
}

// cancelTask stops a running task, or ends a waiting one: nothing runs
// there, so its record is cancelled at once and its Handle is told to stop.
func (m *Middleware) cancelTask(ctx context.Context, in CancelInput, c *tool.Call) (tool.Result, error) {
	j, r, ok := m.lookup(c, in.TaskID)
	switch {
	case !ok:
		return tool.Errorf("unknown task %q", in.TaskID), nil
	case r.Status == StatusWaiting:
		var cancelled []*jobs.Job[*task]
		m.updateRecords(c.State, c.ThreadID, func(r *TaskRecord) {
			if r.ID == in.TaskID && r.Status == StatusWaiting {
				cancelled = append(cancelled, m.cancelWaiting(r)...)
			}
		})
		stopWaiting(ctx, cancelled)
		return tool.Text(fmt.Sprintf("Cancelled task %s.", in.TaskID)), nil
	case r.terminal():
		return tool.Text(fmt.Sprintf("Task %s already ended: %s.", r.ID, r.Status)), nil
	}
	if err := jobs.Stop(ctx, j, DefaultStopWait); err != nil {
		return tool.Errorf("%v", err), nil
	}
	if _, r, _ = m.lookup(c, in.TaskID); r.Status != StatusCancelled {
		// It ended on its own first: the notification brings its result.
		return tool.Text(fmt.Sprintf("Task %s ended before it could be cancelled: %s. You will be notified of its result.", r.ID, r.Status)), nil
	}
	// The model asked for the stop, so it needs no notification about it.
	m.markSeen(c, in.TaskID)
	return tool.Text(fmt.Sprintf("Cancelled task %s.", in.TaskID)), nil
}

// updateTask sends a message to a running task, or continues an ended one
// with it (see continueTask).
func (m *Middleware) updateTask(ctx context.Context, in UpdateInput, c *tool.Call) (tool.Result, error) {
	switch {
	case strings.TrimSpace(in.Message) == "":
		return tool.Errorf("the message must not be empty"), nil
	case len(in.Message) > MaxUpdateBytes:
		return tool.Errorf("the message is %d bytes; the limit is %d", len(in.Message), MaxUpdateBytes), nil
	}
	j, r, ok := m.lookup(c, in.TaskID)
	if !ok {
		return tool.Errorf("unknown task %q", in.TaskID), nil
	}
	var err error
	switch {
	case r.Status == StatusWaiting:
		err = ErrWaiting
	case r.Status == StatusRunning && j != nil:
		err = j.Handle.send(ctx, in.Message)
	case r.terminal():
		return m.continueTask(ctx, c, r, j, in.Message), nil
	default:
		err = ErrFinished
	}
	if err != nil {
		return tool.Errorf("cannot update task %s: %v", in.TaskID, err), nil
	}
	return tool.Text(fmt.Sprintf("Sent the message to task %s; it reads it before its next step.", in.TaskID)), nil
}

// continueTask starts the next episode of the ended task r with message, on
// the task's own history, when its Handle is a Continuer. A task this
// process no longer holds is first reattached when the Runner is a
// Reattacher. The episode runs from the context the task started under (so
// it stays nested in the parent), in the stricter of the task's permission
// mode and the parent run's current one: a parent now in plan mode cannot
// resume a task that writes. It takes a running slot (jobs.Manager.Restart),
// and the record is running again, unannounced.
func (m *Middleware) continueTask(ctx context.Context, c *tool.Call, r TaskRecord, j *jobs.Job[*task], message string) tool.Result {
	f := followUp{backend: c.Backend}
	if info, ok := agent.Info(ctx); ok && info.Agent != nil {
		f.mode = info.Agent.PermissionMode(c.State)
	}
	if j == nil {
		var err error
		if j, err = m.reattach(withFollowUp(ctx, f), r); err != nil {
			return tool.Errorf("cannot update task %s: %v", r.ID, err)
		}
	}
	if _, ok := j.Handle.handle().(Continuer); !ok {
		return tool.Errorf("cannot update task %s: %v", r.ID, ErrFinished)
	}
	_, err := m.jobs.Restart(r.ThreadID, r.ID, func() error {
		return j.Handle.continueWith(withFollowUp(j.Handle.base, f), message)
	})
	var limit *jobs.LimitError
	switch {
	case errors.Is(err, jobs.ErrShutdown):
		return tool.Errorf("background tasks are shut down")
	case errors.As(err, &limit):
		return tool.Errorf("too many background tasks running (limit %d); wait for one to finish or cancel one with %s", limit.Limit, ToolCancel)
	case errors.Is(err, jobs.ErrUnknownJob):
		return tool.Errorf("cannot update task %s: %v", r.ID, errStillRunning)
	case err != nil:
		return tool.Errorf("cannot update task %s: %v", r.ID, err)
	}
	m.updateRecords(c.State, c.ThreadID, func(rec *TaskRecord) {
		if rec.ID == r.ID {
			rec.Status, rec.Ended = StatusRunning, time.Time{}
			rec.Result, rec.Error, rec.Pending, rec.Notified = "", "", nil, false
		}
	})
	return tool.Text(fmt.Sprintf("Task %s continues with your message; you will be notified when it finishes.", r.ID))
}

// reattach takes over the stopped task r, which this process does not hold,
// through the Runner's Reattacher, and registers it under its id. Attach
// calls are serialized, so one task is never attached twice.
func (m *Middleware) reattach(ctx context.Context, r TaskRecord) (*jobs.Job[*task], error) {
	ra, ok := m.runner.(Reattacher)
	if !ok {
		return nil, ErrFinished
	}
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
	if j := m.jobs.Get(r.ThreadID, r.ID); j != nil {
		return j, nil // attached by another run meanwhile
	}
	h, err := ra.Attach(ctx, r)
	if err == nil && h == nil {
		err = errNoHandle
	}
	if err != nil {
		return nil, err
	}
	t := newTask(ctx)
	t.attach(h)
	j, err := m.jobs.Adopt(r.ThreadID, r.ID, r.Started, t)
	if err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultStopWait)
		_ = h.Stop(stopCtx) // nobody holds it
		cancel()
		return nil, err
	}
	if r.Notified {
		m.jobs.MarkNotified(r.ThreadID, r.ID)
	}
	return j, nil
}

func (m *Middleware) listTasks(_ context.Context, _ ListInput, c *tool.Call) (tool.Result, error) {
	records := m.updateRecords(c.State, c.ThreadID, nil)
	if len(records) == 0 {
		return tool.Text("No background tasks."), nil
	}
	now := time.Now()
	entries := make([]string, len(records))
	for i, r := range records {
		entries[i] = listEntry(r, now)
	}
	return tool.Text(strings.Join(entries, "\n\n")), nil
}
