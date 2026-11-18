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
	"github.com/aswin-kevin/orchestrix/backend/shell"
)

// stopWait bounds how long stopping jobs waits for them to end: a job that
// ignores SIGTERM is killed after the backend's grace period, and the second
// period covers reaping what it left behind.
const stopWait = 2 * shell.DefaultKillGrace

// jobIDPrefix starts every job id; the rest is the job's number in its thread.
const jobIDPrefix = "bg_"

// errShutdown refuses jobs once the middleware has been shut down.
var errShutdown = errors.New("background tasks are shut down")

// job is one started command.
type job struct {
	id, threadID, command string
	started               time.Time
	proc                  backend.Process
	// stopped is set before the job is stopped on purpose, so its end is
	// reported as stopped rather than failed.
	stopped atomic.Bool
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
	case j.stopped.Load():
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
		delete(tj.jobs, tj.finished[0])
		tj.finished = tj.finished[1:]
	}
	tj.lastFinished = j.id
	close(tj.changed)
	tj.changed = make(chan struct{})
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
// function reporting the id of the job that ended last.
func (mg *manager) changed(threadID string) (<-chan struct{}, func() string) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	tj := mg.thread(threadID)
	return tj.changed, func() string {
		mg.mu.Lock()
		defer mg.mu.Unlock()
		return tj.lastFinished
	}
}

// stop stops j and waits, up to stopWait or until ctx ends, for it to end.
func stop(ctx context.Context, j *job) error {
	j.stopped.Store(true)
	if err := j.proc.Stop(); err != nil {
		return fmt.Errorf("stop %s: %w", j.id, err)
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
		if !j.done() {
			j.stopped.Store(true)
			_ = j.proc.Stop() // a failed signal still ends in Done via the kill escalation
		}
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
// stopWait.
func (mg *manager) shutdown() {
	mg.mu.Lock()
	mg.closed = true
	var jobs []*job
	for _, tj := range mg.threads {
		for _, j := range tj.jobs {
			jobs = append(jobs, j)
		}
	}
	mg.threads = map[string]*threadJobs{}
	mg.mu.Unlock()
	if stopAll(jobs) {
		mg.watchers.Wait()
	}
}
