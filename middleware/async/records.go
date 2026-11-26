package async

import (
	"slices"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/jobs"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
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
	// TaskThreadID is the thread the task runs on (Request.TaskThreadID);
	// empty in a record that predates it.
	TaskThreadID string `json:"task_thread_id,omitempty"`
	// SubagentType is the name of the subagent the task runs.
	SubagentType string `json:"subagent_type"`
	// Description is the task as the model wrote it.
	Description string `json:"description"`
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
	// is StatusWaiting.
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
// takes its outcome; one whose task the middleware does not know is lost.
// Records of other threads are kept untouched. edit may be nil.
func (m *Middleware) updateRecords(s *state.State, threadID string, edit func(*TaskRecord)) []TaskRecord {
	return jobs.UpdateRecords(s, tasksKey, threadID, m.jobs.MaxFinished(), recordAccessors, func(r *TaskRecord) {
		m.refresh(r)
		if edit != nil {
			edit(r)
		}
	})
}

// refresh updates a running or waiting record from its task. A waiting
// record whose job the middleware no longer holds stays waiting: it is not
// lost, as nothing was running.
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
		return
	}
	if j.Stopped() && out.Status == StatusFailed {
		// A runner that fails a stopped task means it was cancelled.
		out.Status = StatusCancelled
	}
	r.Status, r.Pending = out.Status, out.Pending
	r.Ended = time.Time{}
	if out.Status.terminal() {
		r.Ended = ended
	}
	r.Result, _ = promptsafe.CapBytes(out.Text, MaxRecordedResult)
	r.Error, _ = promptsafe.CapBytes(out.Error, MaxRecordedResult)
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
