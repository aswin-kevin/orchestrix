// Package jobs is a thread-scoped registry of background work. Each thread
// numbers its jobs prefix+N, with numbers that never repeat even when the
// thread's entry was released and the process restarted (the caller passes the
// ids it recorded). A thread has a running limit and a bounded history of
// finished jobs that forgets announced jobs first; the registry offers a change
// signal, release, stop-and-forget, and a bounded shutdown.
//
// It is generic over the job handle, so shell tasks and background subagents
// share it. All Manager methods are safe for concurrent use.
package jobs

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
)

// Handle is one unit of background work.
type Handle interface {
	// Done is closed once the work has ended.
	Done() <-chan struct{}
	// Stop asks the work to end; it is a no-op once the work ended.
	Stop() error
}

// Limits bounds a Manager. Both fields must be positive; callers apply their
// own defaults before constructing it.
type Limits struct {
	// MaxRunning is how many jobs may run at once in one thread.
	MaxRunning int
	// MaxFinished is how many ended jobs are remembered per thread.
	MaxFinished int
}

// ErrShutdown is returned by Start once the Manager was shut down.
var ErrShutdown = errors.New("jobs: shut down")

// LimitError reports a thread at its running limit.
type LimitError struct {
	// Limit is the running limit that was reached.
	Limit int
}

// Error implements error.
func (e *LimitError) Error() string {
	return fmt.Sprintf("jobs: thread already runs %d jobs", e.Limit)
}

// Job is a started Handle with its identity in the registry.
type Job[H Handle] struct {
	// ID is the job's id, prefix+N.
	ID string
	// ThreadID is the thread the job belongs to.
	ThreadID string
	// Started is when the job was registered.
	Started time.Time
	// Handle is the work itself.
	Handle H

	// stopped is set before a running job is stopped on purpose, so its end
	// can be reported as stopped rather than failed.
	stopped atomic.Bool
	// notified is set once the model was told the job ended; the thread's
	// entry may be dropped when every job it holds is.
	notified atomic.Bool
}

// Ended reports whether the job's Done channel is closed.
func (j *Job[H]) Ended() bool {
	select {
	case <-j.Handle.Done():
		return true
	default:
		return false
	}
}

// Stopped reports whether the job was stopped on purpose (see RequestStop).
func (j *Job[H]) Stopped() bool { return j.stopped.Load() }

// Notified reports whether MarkNotified was called for the job.
func (j *Job[H]) Notified() bool { return j.notified.Load() }

// thread holds the jobs of one thread.
type thread[H Handle] struct {
	// next is the number of the last job started in the thread, including
	// one whose start is still in flight.
	next int
	// starting counts the starts in flight: each holds a slot of the
	// running limit and keeps the entry from being dropped.
	starting int
	jobs     map[string]*Job[H]
	// finished lists the ids of ended jobs still remembered, oldest first.
	finished []string
	// changed is closed, and replaced, whenever a job of the thread ends;
	// lastFinished is the id of that job.
	changed      chan struct{}
	lastFinished string
}

func newThread[H Handle]() *thread[H] {
	return &thread[H]{jobs: map[string]*Job[H]{}, changed: make(chan struct{})}
}

// settled reports whether the model was told about the end of every job.
func (t *thread[H]) settled() bool {
	for _, j := range t.jobs {
		if !j.Notified() {
			return false
		}
	}
	return true
}

func (t *thread[H]) running() int {
	n := 0
	for _, j := range t.jobs {
		if !j.Ended() {
			n++
		}
	}
	return n
}

// evictOne forgets the oldest ended job the model was told about, or the
// oldest ended job when it was told about none, so a job whose end has not
// been announced yet is not reported as lost while others can go first.
func (t *thread[H]) evictOne() {
	i := slices.IndexFunc(t.finished, func(id string) bool { return t.jobs[id].Notified() })
	i = max(i, 0)
	delete(t.jobs, t.finished[i])
	t.finished = slices.Delete(t.finished, i, i+1)
}

// Manager owns the jobs of every thread. It is shared by concurrent runs, so
// every field is guarded by mu.
type Manager[H Handle] struct {
	prefix string
	limits Limits

	mu       sync.Mutex
	threads  map[string]*thread[H]
	closed   bool
	watchers sync.WaitGroup
	// starting tracks the starts in flight, so Shutdown can wait for them.
	// Add happens under mu and only before closed is set.
	starting sync.WaitGroup
}

// NewManager returns a Manager that numbers ids prefix+N per thread. It panics
// when either limit is not positive, which is a programming error.
func NewManager[H Handle](prefix string, l Limits) *Manager[H] {
	if l.MaxRunning <= 0 || l.MaxFinished <= 0 {
		panic(fmt.Sprintf("jobs: limits must be positive, got %+v", l))
	}
	return &Manager[H]{prefix: prefix, limits: l, threads: map[string]*thread[H]{}}
}

// MaxFinished returns the number of ended jobs remembered per thread.
func (m *Manager[H]) MaxFinished() int { return m.limits.MaxFinished }

// ThreadCount returns how many threads the Manager keeps an entry for.
func (m *Manager[H]) ThreadCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.threads)
}

// thread returns the entry of threadID, creating it; mu must be held.
func (m *Manager[H]) thread(threadID string) *thread[H] {
	t, ok := m.threads[threadID]
	if !ok {
		t = newThread[H]()
		m.threads[threadID] = t
	}
	return t
}

// Start registers the handle start returns as the next job of threadID.
// recordedIDs lists the ids the caller already recorded, so a thread resumed in
// a new process never reuses one: numbering continues after the highest number
// among them.
//
// Start first reserves the job under the Manager's lock: the reservation
// takes the job's number and holds a slot of the thread's running limit. It
// then calls start without the lock, so a slow start (a container spawn, say)
// stalls no other caller; start may block, but must not call the Manager.
// Once start returns, the job is committed or, when start failed, the
// reservation is rolled back: the slot is freed and the number is given to
// the next job unless a later reservation already took a higher one, in which
// case it is skipped. A job committed after Shutdown began is stopped at once
// and returned with Stopped set, so no work outlives Shutdown.
//
// Start returns ErrShutdown after Shutdown and a *LimitError when the thread
// runs or is starting MaxRunning jobs; an error from start is returned as is
// and registers nothing.
func (m *Manager[H]) Start(threadID string, recordedIDs []string, start func() (H, error)) (*Job[H], error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrShutdown
	}
	t := m.thread(threadID)
	if t.running()+t.starting >= m.limits.MaxRunning {
		m.mu.Unlock()
		return nil, &LimitError{Limit: m.limits.MaxRunning}
	}
	t.next = max(t.next, m.highestNumber(recordedIDs)) + 1
	n := t.next
	t.starting++
	m.starting.Add(1)
	m.mu.Unlock()
	committed := false
	defer func() {
		if !committed { // start failed or panicked
			m.mu.Lock()
			t.starting--
			if t.next == n {
				t.next-- // nothing took a later number: hand this one out again
			}
			m.mu.Unlock()
		}
		m.starting.Done()
	}()

	h, err := start()
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	t.starting--
	committed = true
	j := &Job[H]{ID: m.prefix + strconv.Itoa(n), ThreadID: threadID, Started: time.Now(), Handle: h}
	t.jobs[j.ID] = j
	m.watchers.Add(1)
	go m.watch(t, j)
	closed := m.closed
	m.mu.Unlock()
	if closed {
		_, _ = RequestStop(j) // a failed signal still ends in Done via the handle's own escalation
	}
	return j, nil
}

// highestNumber returns the largest job number among ids; 0 when none parses.
func (m *Manager[H]) highestNumber(ids []string) int {
	highest := 0
	for _, id := range ids {
		if n, err := strconv.Atoi(strings.TrimPrefix(id, m.prefix)); err == nil {
			highest = max(highest, n)
		}
	}
	return highest
}

// watch waits for j to end, then wakes the thread's sleepers and forgets the
// oldest ended jobs beyond MaxFinished. A job already dropped (its nested run
// ended) needs neither.
func (m *Manager[H]) watch(t *thread[H], j *Job[H]) {
	defer m.watchers.Done()
	<-j.Handle.Done()
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.jobs[j.ID] != j {
		return
	}
	t.finished = append(t.finished, j.ID)
	for len(t.finished) > m.limits.MaxFinished {
		t.evictOne()
	}
	t.lastFinished = j.ID
	close(t.changed)
	t.changed = make(chan struct{})
}

// Get returns the job id of threadID, or nil when the Manager does not know it.
func (m *Manager[H]) Get(threadID, id string) *Job[H] {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.threads[threadID]; ok {
		return t.jobs[id]
	}
	return nil
}

// Changed returns a channel closed when the next job of threadID ends, and a
// function reporting the id of the job that ended last. A thread without jobs
// gets a nil channel, which never fires: nothing can end there.
func (m *Manager[H]) Changed(threadID string) (<-chan struct{}, func() (lastEnded string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.threads[threadID]
	if !ok {
		return nil, func() string { return "" }
	}
	return t.changed, func() string {
		m.mu.Lock()
		defer m.mu.Unlock()
		return t.lastFinished
	}
}

// MarkNotified records that the model was told the jobs ids of threadID ended.
// Unknown ids are ignored.
func (m *Manager[H]) MarkNotified(threadID string, ids ...string) {
	for _, id := range ids {
		if j := m.Get(threadID, id); j != nil {
			j.notified.Store(true)
		}
	}
}

// Release drops the entry of threadID once no job runs there and the model was
// told about every job's end, so memory stays bounded by the threads with
// something left to report. A released thread starts its next ids after the
// highest one the caller's records hold (see Start).
func (m *Manager[H]) Release(threadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.threads[threadID]; ok && t.running() == 0 && t.starting == 0 && t.settled() {
		delete(m.threads, threadID)
	}
}

// RequestStop marks j stopped and calls its Stop when it is still running,
// reporting whether it was. A job that already ended keeps its outcome.
func RequestStop[H Handle](j *Job[H]) (bool, error) {
	if j.Ended() {
		return false, nil
	}
	j.stopped.Store(true)
	if err := j.Handle.Stop(); err != nil {
		return true, fmt.Errorf("stop %s: %w", j.ID, err)
	}
	return true, nil
}

// Stop requests a stop and waits up to wait, or until ctx ends, for j to end.
func Stop[H Handle](ctx context.Context, j *Job[H], wait time.Duration) error {
	if j == nil {
		return errors.New("stop: no such job")
	}
	if _, err := RequestStop(j); err != nil {
		return err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-j.Handle.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("stop %s: still running after %s", j.ID, wait)
	}
}

// stopAll stops jobs concurrently and waits for them all, sharing one wait
// deadline; it reports whether every job ended.
func stopAll[H Handle](list []*Job[H], wait time.Duration) bool {
	for _, j := range list {
		_, _ = RequestStop(j) // a failed signal still ends in Done via the handle's own escalation
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for _, j := range list {
		select {
		case <-j.Handle.Done():
		case <-deadline.C:
			return false
		}
	}
	return true
}

// StopAndForget stops the jobs with ids in threadID, waits for them up to wait,
// and then drops them, and the thread once it has no jobs left and none
// starting. refresh runs after the jobs ended (or wait passed) but before they
// are dropped, so it can still read their outcome.
func (m *Manager[H]) StopAndForget(threadID string, ids []string, wait time.Duration, refresh func()) {
	var list []*Job[H]
	for _, id := range ids {
		if j := m.Get(threadID, id); j != nil {
			list = append(list, j)
		}
	}
	_ = stopAll(list, wait) // a job that outlives wait is still dropped: its run is over
	refresh()
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.threads[threadID]
	if !ok {
		return
	}
	for _, j := range list {
		delete(t.jobs, j.ID)
		t.finished = slices.DeleteFunc(t.finished, func(id string) bool { return id == j.ID })
	}
	if len(t.jobs) == 0 && t.starting == 0 {
		delete(m.threads, threadID)
	}
}

// Shutdown refuses new jobs, stops every job and waits for them, up to wait.
// It first waits, within the same wait, for the starts in flight: each one
// commits its job and stops it at once (see Start), so it is stopped and
// waited for with the others. A start still in flight when wait passes stops
// its job as it commits, after Shutdown returned. The entries are kept, so
// records refreshed afterwards show the jobs as stopped rather than lost.
func (m *Manager[H]) Shutdown(wait time.Duration) {
	deadline := time.Now().Add(wait)
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	started := make(chan struct{})
	go func() { m.starting.Wait(); close(started) }()
	timer := time.NewTimer(wait)
	select {
	case <-started:
	case <-timer.C:
	}
	timer.Stop()

	m.mu.Lock()
	var list []*Job[H]
	for _, t := range m.threads {
		for _, j := range t.jobs {
			list = append(list, j)
		}
	}
	m.mu.Unlock()
	if stopAll(list, time.Until(deadline)) {
		m.watchers.Wait()
	}
}
