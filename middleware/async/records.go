package async

import (
	"slices"
	"time"

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
	// SubagentType is the name of the subagent the task runs.
	SubagentType string `json:"subagent_type"`
	// Description is the task as the model wrote it.
	Description string `json:"description"`
	// Status is one of the Status constants.
	Status string `json:"status"`
	// Started is when the task was started.
	Started time.Time `json:"started"`
	// Ended is when the task ended, or for a lost task when that was
	// noticed; zero while it runs.
	Ended time.Time `json:"ended,omitzero"`
	// Result is the task's final answer, at most MaxRecordedResult bytes,
	// kept so check_task can show it after the live task was released.
	Result string `json:"result,omitempty"`
	// Error says why a task that did not complete ended, at most
	// MaxRecordedResult bytes.
	Error string `json:"error,omitempty"`
	// Notified reports that the model was told the task ended, by a
	// notification, by check_task or because it cancelled the task itself.
	Notified bool `json:"notified,omitempty"`
}

// TasksKey holds the run's task records, oldest first. It is run-local
// (state.RunLocal): neither a subagent nor a fork inherits the records, since
// the tasks belong to the thread that started them.
var TasksKey = state.NewKey[[]TaskRecord]("async.tasks", state.RunLocal[[]TaskRecord]())

// finished reports whether the record describes an ended task.
func (r TaskRecord) finished() bool { return r.Status != StatusRunning }

// Thread implements jobs.Record: it returns the record's ThreadID.
func (r TaskRecord) Thread() string { return r.ThreadID }

// Settled implements jobs.Record: it reports whether the task ended and the
// model was told so, which makes the record one that may be pruned.
func (r TaskRecord) Settled() bool { return r.finished() && r.Notified }

// recordedIDs returns the ids of the run's records.
func recordedIDs(s *state.State) []string {
	records, _ := TasksKey.Get(s)
	ids := make([]string, len(records))
	for i, r := range records {
		ids[i] = r.ID
	}
	return ids
}

// addRecord appends the record of a task that just started.
func addRecord(s *state.State, r TaskRecord) {
	TasksKey.Update(s, func(records []TaskRecord) []TaskRecord {
		return append(slices.Clone(records), r)
	})
}

// updateRecords brings the run's records of threadID up to date with the
// tasks, then lets edit change each of them, in one State update, and returns
// a copy of the records of threadID. A running record whose task ended takes
// its outcome; one whose task the middleware does not know is lost. Records
// of other threads are kept untouched. edit may be nil.
func (m *Middleware) updateRecords(s *state.State, threadID string, edit func(*TaskRecord)) []TaskRecord {
	return jobs.UpdateRecords(s, TasksKey, threadID, m.jobs.MaxFinished(), func(r *TaskRecord) {
		m.refresh(r)
		if edit != nil {
			edit(r)
		}
	})
}

// refresh updates a running record from its task.
func (m *Middleware) refresh(r *TaskRecord) {
	if r.finished() {
		return
	}
	j := m.jobs.Get(r.ThreadID, r.ID)
	if j == nil {
		r.Status, r.Ended = StatusLost, time.Now()
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
	r.Status, r.Ended = out.Status, ended
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
		if r.ID == id && r.finished() {
			m.markNotified(r)
		}
	})
}
