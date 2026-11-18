package tasks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

// stopWait bounds how long stopping jobs waits for them to end: a job that
// ignores SIGTERM is killed after the backend's grace period, and the second
// period covers reaping what it left behind.
const stopWait = 2 * backend.DefaultKillGrace

// jobIDPrefix starts every job id; the rest is the job's number in its thread.
const jobIDPrefix = "bg_"

// errShutdown refuses jobs once the middleware has been shut down.
var errShutdown = errors.New("background tasks are shut down")

// job is one started command.
type job struct {
	id, threadID, command string
	started               time.Time
	proc                  backend.Process
	// stopped is set before a running job is stopped on purpose, so its end
	// is reported as stopped rather than failed.
	stopped atomic.Bool
	// notified is set once the model was told the job ended; the thread's
	// entry may be dropped when every job it holds is.
	notified atomic.Bool
}

// done reports whether the job has ended.
func (j *job) done() bool {
	_, ok := j.proc.Result()
	return ok
}

// outcome returns the job's status and exit code; the exit code is 0 while
// the job runs.
func (j *job) outcome() (status string, exitCode int) {
	res, ok := j.proc.Result()
	switch {
	case !ok:
		return StatusRunning, 0
	case j.stopped.Load() && res.ExitCode != 0:
		// A job that exited cleanly as the stop raced it completed.
		return StatusStopped, res.ExitCode
	case res.TimedOut:
		return StatusTimedOut, res.ExitCode
	case res.ExitCode == 0:
		return StatusCompleted, 0
	}
	return StatusFailed, res.ExitCode
}

// threadJobs holds the jobs of one thread.
type threadJobs struct {
	// next is the number of the last job started in the thread.
	next int
	jobs map[string]*job
	// finished lists the ids of ended jobs still remembered, oldest first.
	finished []string
	// changed is closed, and replaced, whenever a job of the thread ends;
	// lastFinished is the id of that job.
	changed      chan struct{}
	lastFinished string
}

func newThreadJobs() *threadJobs {
	return &threadJobs{jobs: map[string]*job{}, changed: make(chan struct{})}
}

// settled reports whether the model was told about the end of every job.
func (tj *threadJobs) settled() bool {
	for _, j := range tj.jobs {
		if !j.notified.Load() {
			return false
		}
	}
	return true
}

func (tj *threadJobs) running() int {
	n := 0
	for _, j := range tj.jobs {
		if !j.done() {
			n++
		}
	}
	return n
}

// manager owns every job of every thread. It is shared by the concurrent runs
// of the agents using the middleware, so every field is guarded by mu.
type manager struct {
	maxRunning, maxFinished int

	mu       sync.Mutex
	threads  map[string]*threadJobs
	closed   bool
	watchers sync.WaitGroup
}

func newManager(maxRunning, maxFinished int) *manager {
	return &manager{maxRunning: maxRunning, maxFinished: maxFinished, threads: map[string]*threadJobs{}}
}

// thread returns the jobs of threadID, creating the entry; mu must be held.
func (mg *manager) thread(threadID string) *threadJobs {
	tj, ok := mg.threads[threadID]
	if !ok {
		tj = newThreadJobs()
		mg.threads[threadID] = tj
	}
	return tj
}

// start launches command as the next job of threadID. recorded lists the
// jobs the run's State already knows, so a thread resumed in a new process
// never reuses the id of a job it recorded before. The lock is held across
// the start so that the running limit and Shutdown cannot be raced.
func (mg *manager) start(ctx context.Context, starter backend.Starter, threadID, command string, timeout time.Duration, recorded []JobRecord) (*job, error) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if mg.closed {
		return nil, errShutdown
	}
	tj := mg.thread(threadID)
	if tj.running() >= mg.maxRunning {
		return nil, fmt.Errorf("too many background tasks running (limit %d); stop one with %s or wait for one to finish", mg.maxRunning, ToolTaskStop)
	}
	proc, err := starter.Start(ctx, command, backend.ExecOptions{Timeout: timeout})
	if err != nil {
		return nil, err
	}
	tj.next = max(tj.next, highestJobNumber(recorded)) + 1
	j := &job{id: jobIDPrefix + strconv.Itoa(tj.next), threadID: threadID, command: command, started: time.Now(), proc: proc}
	tj.jobs[j.id] = j
	mg.watchers.Add(1)
	go mg.watch(tj, j)
	return j, nil
}

// highestJobNumber returns the largest job number among records; 0 when
// there is none.
func highestJobNumber(records []JobRecord) int {
	highest := 0
	for _, r := range records {
		if n, err := strconv.Atoi(strings.TrimPrefix(r.ID, jobIDPrefix)); err == nil {
			highest = max(highest, n)
		}
	}
	return highest
}

// watch waits for j to end, then wakes the thread's sleepers and forgets the
// oldest ended jobs beyond maxFinished. A job already dropped (its nested run
// ended, or the manager shut down) needs neither.
func (mg *manager) watch(tj *threadJobs, j *job) {
	defer mg.watchers.Done()
	<-j.proc.Done()
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if tj.jobs[j.id] != j {
		return
	}
	tj.finished = append(tj.finished, j.id)
	for len(tj.finished) > mg.maxFinished {
		tj.evictOne()
	}
	tj.lastFinished = j.id
	close(tj.changed)
	tj.changed = make(chan struct{})
}

// evictOne forgets the oldest ended job the model was told about, or the
// oldest ended job when it was told about none, so a job whose end has not
// been announced yet is not reported as lost while others can go first.
func (tj *threadJobs) evictOne() {
	i := slices.IndexFunc(tj.finished, func(id string) bool { return tj.jobs[id].notified.Load() })
	i = max(i, 0)
	delete(tj.jobs, tj.finished[i])
	tj.finished = slices.Delete(tj.finished, i, i+1)
}

// job returns the job id of threadID, or nil when the manager does not know
// it.
func (mg *manager) job(threadID, id string) *job {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if tj, ok := mg.threads[threadID]; ok {
		return tj.jobs[id]
	}
	return nil
}

// changed returns a channel closed when the next job of threadID ends, and a
// function reporting the id of the job that ended last. A thread without
// jobs gets a nil channel, which never fires: nothing can end there.
func (mg *manager) changed(threadID string) (<-chan struct{}, func() string) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	tj, ok := mg.threads[threadID]
	if !ok {
		return nil, func() string { return "" }
	}
	return tj.changed, func() string {
		mg.mu.Lock()
		defer mg.mu.Unlock()
		return tj.lastFinished
	}
}

// markNotified records that the model was told the jobs ids of threadID
// ended.
func (mg *manager) markNotified(threadID string, ids []string) {
	for _, id := range ids {
		if j := mg.job(threadID, id); j != nil {
			j.notified.Store(true)
		}
	}
}

// release drops the entry of threadID once no job runs there and the model
// was told about every job's end, so memory stays bounded by the threads
// with something left to report. A thread released this way starts its
// next ids after the highest one its records hold (see start).
func (mg *manager) release(threadID string) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if tj, ok := mg.threads[threadID]; ok && tj.running() == 0 && tj.settled() {
		delete(mg.threads, threadID)
	}
}

// requestStop stops j if it is still running, reporting whether it was. A
// job that already ended keeps its outcome.
func requestStop(j *job) (bool, error) {
	if j.done() {
		return false, nil
	}
	j.stopped.Store(true)
	if err := j.proc.Stop(); err != nil {
		return true, fmt.Errorf("stop %s: %w", j.id, err)
	}
	return true, nil
}

// stop stops j and waits, up to stopWait or until ctx ends, for it to end.
func stop(ctx context.Context, j *job) error {
	if j == nil {
		return errors.New("stop: no such job")
	}
	if _, err := requestStop(j); err != nil {
		return err
	}
	timer := time.NewTimer(stopWait)
	defer timer.Stop()
	select {
	case <-j.proc.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("stop %s: still running after %s", j.id, stopWait)
	}
}

// stopAll stops jobs concurrently and waits for them all, sharing one
// stopWait deadline; it reports whether every job ended.
func stopAll(jobs []*job) bool {
	for _, j := range jobs {
		_, _ = requestStop(j) // a failed signal still ends in Done via the kill escalation
	}
	deadline := time.NewTimer(stopWait)
	defer deadline.Stop()
	for _, j := range jobs {
		select {
		case <-j.proc.Done():
		case <-deadline.C:
			return false
		}
	}
	return true
}

// stopAndForget stops the jobs with ids in threadID, waits for them, and
// then drops them, and the thread once it has no jobs left. refresh runs
// after the jobs ended but before they are dropped, so it can still read
// their outcome.
func (mg *manager) stopAndForget(threadID string, ids []string, refresh func()) {
	var jobs []*job
	for _, id := range ids {
		if j := mg.job(threadID, id); j != nil {
			jobs = append(jobs, j)
		}
	}
	_ = stopAll(jobs) // a job that outlives stopWait is still dropped: its run is over
	refresh()
	mg.mu.Lock()
	defer mg.mu.Unlock()
	tj, ok := mg.threads[threadID]
	if !ok {
		return
	}
	for _, j := range jobs {
		delete(tj.jobs, j.id)
		tj.finished = slices.DeleteFunc(tj.finished, func(id string) bool { return id == j.id })
	}
	if len(tj.jobs) == 0 {
		delete(mg.threads, threadID)
	}
}

// shutdown refuses new jobs, stops every job and waits for them, up to
// stopWait. The entries are kept, so records refreshed afterwards show the
// jobs as stopped rather than lost.
func (mg *manager) shutdown() {
	mg.mu.Lock()
	mg.closed = true
	var jobs []*job
	for _, tj := range mg.threads {
		for _, j := range tj.jobs {
			jobs = append(jobs, j)
		}
	}
	mg.mu.Unlock()
	if stopAll(jobs) {
		mg.watchers.Wait()
	}
}
