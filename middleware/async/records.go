package async

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/jobs"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// TaskRecord is a run's record of one background task.
type TaskRecord struct {
	// ID is the task's id, the one the model passes to check_task,
	// update_task and cancel_task.
	ID string `json:"id"`
	// ThreadID is the thread of the run that started the task.
	ThreadID string `json:"thread_id"`
	// TaskThreadID is the thread the task runs on (Request.TaskThreadID).
	TaskThreadID string `json:"task_thread_id,omitempty"`
	// SubagentType is the name of the subagent the task runs.
	SubagentType string `json:"subagent_type"`
	// Description is the task as the model wrote it.
	Description string `json:"description"`
	// StartMode is the parent run's permission mode when it started the
	// task. A follow-up or decision through a Runner other than the
	// built-in one is refused while the parent's mode is stricter (see
	// FollowUpMode); empty in records saved before it existed, which then
	// count as permission.ModeDefault.
	StartMode permission.Mode `json:"start_mode,omitempty"`
	// Status is one of the Status constants.
	Status Status `json:"status"`
	// Started is when the task was started.
	Started time.Time `json:"started"`
	// Ended is when the task ended, or for a lost task when that was
	// noticed; zero while it runs or waits.
	Ended time.Time `json:"ended,omitzero"`
	// Result is the task's final answer, at most MaxRecordedResult bytes,
	// kept so check_task can show it after the live task was released.
	Result string `json:"result,omitempty"`
	// Error says why a task that did not complete ended, at most
	// MaxRecordedResult bytes.
	Error string `json:"error,omitempty"`
	// Pending is what a waiting task needs a decision on; nil unless Status
	// is StatusWaiting. It is a copy for display: each request's Input and
	// Reason are cut to MaxRecordedResult bytes (an Input cut short is
	// recorded as a JSON string of its first bytes), since records live in
	// State; the task's own thread keeps the pending calls in full.
	Pending *agent.Interrupt `json:"pending,omitempty"`
	// Notified reports that the model was told the task ended, by a
	// notification, by check_task or because it cancelled the task itself;
	// for a waiting task, that it was told the task waits.
	Notified bool `json:"notified,omitempty"`
}

// tasksKey holds the run's task records, oldest first. It is run-local
// (state.RunLocal): neither a subagent nor a fork inherits the records, since
// the tasks belong to the thread that started them.
var tasksKey = state.NewKey[[]TaskRecord]("async.tasks", state.RunLocal[[]TaskRecord]())

// Tasks returns a copy of the task records in s, oldest first, of every
// thread whose runs used s. It returns nil when s holds none.
func Tasks(s *state.State) []TaskRecord {
	records, _ := tasksKey.Get(s)
	return slices.Clone(records)
}

// terminal reports whether the record describes a task that ended for good.
func (r TaskRecord) terminal() bool { return r.Status.terminal() }

// recordAccessors lets the jobs record helpers read a TaskRecord: a record
// is settled, and so may be pruned, once the task ended for good and the
// model was told so. A waiting record is never settled.
var recordAccessors = jobs.Accessors[TaskRecord]{
	Thread:  func(r TaskRecord) string { return r.ThreadID },
	Settled: func(r TaskRecord) bool { return r.terminal() && r.Notified },
}

// recordedIDs returns the ids of the run's records.
func recordedIDs(s *state.State) []string {
	records, _ := tasksKey.Get(s)
	ids := make([]string, len(records))
	for i, r := range records {
		ids[i] = r.ID
	}
	return ids
}

// addRecord appends the record of a task that just started.
func addRecord(s *state.State, r TaskRecord) {
	tasksKey.Update(s, func(records []TaskRecord) []TaskRecord {
		return append(slices.Clone(records), r)
	})
}

// updateRecords brings the run's records of threadID up to date with the
// tasks, then lets edit change each of them, in one State update, and returns
// a copy of the records of threadID. A running record whose episode stopped
// takes its outcome; one whose task the middleware does not know (and
// could not reattach when the run started, see BeforeAgent) is lost.
// Records of other threads are kept untouched. edit may be nil.
func (m *Middleware) updateRecords(s *state.State, threadID string, edit func(*TaskRecord)) []TaskRecord {
	return jobs.UpdateRecords(s, tasksKey, threadID, m.jobs.MaxFinished(), m.acc, func(r *TaskRecord) {
		m.refresh(r)
		if edit != nil {
			edit(r)
		}
	})
}

// refresh updates a running or waiting record from its task. A waiting
// record whose job the middleware no longer holds stays waiting: it is not
// lost, as nothing was running. A waiting record whose task was resumed by a
// decision (see Resolve) is running again, and once its task stopped anew
// (it ended, was cancelled, or waits again, even on the same tool call) it
// is to be announced again: Notified, which said the model was told it
// waits, is cleared.
func (m *Middleware) refresh(r *TaskRecord) {
	if r.terminal() {
		return
	}
	j := m.jobs.Get(r.ThreadID, r.ID)
	if j == nil {
		if r.Status == StatusRunning {
			r.Status, r.Ended = StatusLost, time.Now()
		}
		return
	}
	out, ended, ok := j.Handle.outcome()
	if !ok {
		if r.Status == StatusWaiting {
			r.Status, r.Pending, r.Notified = StatusRunning, nil, false
			r.Result, r.Error = "", ""
		}
		return
	}
	out, ended = m.stoppedOutcome(j, out, ended)
	if r.Status == StatusWaiting && (out.Status != StatusWaiting || !samePending(r.Pending, out.Pending) || j.Handle.waitUnseen()) {
		r.Notified = false
	}
	applyOutcome(r, out, ended)
}

// stoppedOutcome is how the episode of j that stopped with out at ended is
// recorded: a runner that fails a stopped task means it was cancelled, and a
// stop that raced an interrupt cancelled the task rather than leaving it
// waiting (its Handle was already asked to stop).
func (m *Middleware) stoppedOutcome(j *jobs.Job[*task], out Outcome, ended time.Time) (Outcome, time.Time) {
	switch {
	case !j.Stopped():
	case out.Status == StatusFailed:
		out.Status = StatusCancelled
	case out.Status == StatusWaiting && j.Handle.cancelWaiting():
		if now, at, ok := j.Handle.outcome(); ok {
			out, ended = now, at
		}
	}
	return out, ended
}

// applyOutcome sets the outcome fields of r from out, which stopped at
// ended, capping what a record keeps.
func applyOutcome(r *TaskRecord, out Outcome, ended time.Time) {
	r.Status, r.Pending = out.Status, recordedPending(out.Pending)
	r.Ended = time.Time{}
	if out.Status.terminal() {
		r.Ended = ended
	}
	r.Result, _ = promptsafe.CapBytes(out.Text, MaxRecordedResult)
	r.Error, _ = promptsafe.CapBytes(out.Error, MaxRecordedResult)
}

// samePending reports whether a and b wait for the same tool calls.
func samePending(a, b *agent.Interrupt) bool {
	if a == nil || b == nil {
		return a == b
	}
	return slices.EqualFunc(a.Requests, b.Requests, func(x, y agent.ActionRequest) bool { return x.ToolUseID == y.ToolUseID })
}

// recordedPending returns the copy of p a record keeps (see
// TaskRecord.Pending); nil for nil.
func recordedPending(p *agent.Interrupt) *agent.Interrupt {
	if p == nil {
		return nil
	}
	out := *p
	out.Requests = make([]agent.ActionRequest, len(p.Requests))
	for i, req := range p.Requests {
		req.Reason, _ = promptsafe.CapBytes(req.Reason, MaxRecordedResult)
		if len(req.Input) > MaxRecordedResult {
			text, _ := promptsafe.CapBytes(string(req.Input), MaxRecordedResult)
			req.Input, _ = json.Marshal(text) // a string always encodes
		}
		req.Allowed = slices.Clone(req.Allowed)
		out.Requests[i] = req
	}
	return &out
}

// lookup finds a task of the call's thread: its refreshed record and, while
// the middleware still holds it, the live job.
func (m *Middleware) lookup(c *tool.Call, id string) (*jobs.Job[*task], TaskRecord, bool) {
	for _, r := range m.updateRecords(c.State, c.ThreadID, nil) {
		if r.ID == id {
			return m.jobs.Get(c.ThreadID, id), r, true
		}
	}
	return nil, TaskRecord{}, false
}

// markSeen records that the model was told task id ended.
func (m *Middleware) markSeen(c *tool.Call, id string) {
	m.updateRecords(c.State, c.ThreadID, func(r *TaskRecord) {
		if r.ID == id && r.terminal() {
			m.markNotified(r)
		}
	})
}

// isTaskThread reports whether threadID is, or lies below, the thread of a
// background task: a segment after its first is IDPrefix followed by a
// number, as in <thread>/async_N.
func isTaskThread(threadID string) bool {
	segs := strings.Split(threadID, "/")
	for _, seg := range segs[1:] {
		if n, ok := strings.CutPrefix(seg, IDPrefix); ok && n != "" && strings.Trim(n, "0123456789") == "" {
			return true
		}
	}
	return false
}
