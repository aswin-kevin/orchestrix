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

	"github.com/aswin-kevin/orchestrix/backend"
)

// Handle is one unit of background work.
type Handle interface {
	// Done is closed once the work has ended.
	Done() <-chan struct{}
	// Stop asks the work to end; it is a no-op once the work ended. ctx
	// bounds the request itself, not the wait for Done.
	Stop(ctx context.Context) error
}

// stopRequestTimeout bounds a stop request the Manager sends on its own,
// outside any caller's call: at Close, and for a job committed after it.
const stopRequestTimeout = backend.DefaultKillGrace

// detachedStop returns a context for a stop request that must go out even
// though ctx ended (or the caller has none): it keeps ctx's values, drops
// its cancellation, and is bounded by stopRequestTimeout.
func detachedStop(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stopRequestTimeout)
}

// Limits bounds a Manager. Both fields must be positive; callers apply their
// own defaults before constructing it.
type Limits struct {
	// MaxRunning is how many jobs may run at once in one thread.
	MaxRunning int
	// MaxFinished is how many ended jobs are remembered per thread.
	MaxFinished int
}

// ErrShutdown is returned by Start, Restart and Adopt once the Manager was
// closed.
var ErrShutdown = errors.New("jobs: shut down")

// ErrUnknownJob is returned by Restart for a job the Manager does not hold,
// or one that is still running or already being restarted.
var ErrUnknownJob = errors.New("jobs: unknown job")

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
	// restarting is set, under the Manager's lock, while Restart runs the
	// job again: the job then holds a reservation instead of counting as
	// running, and no other Restart may take it.
	restarting bool
	// episode counts the times Restart took the job, under the Manager's
	// lock. A watcher records the end of the episode it was started for
	// only while the count is unchanged: a Restart that took the job
	// before that watcher ran (its Done was already closed) owns the job
	// from then on.
	episode uint64
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

// Stopped reports whether the job was stopped on purpose (see Stop).
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

// running counts the jobs that run; a job being restarted is counted by
// starting instead. The Manager's lock must be held.
func (t *thread[H]) running() int {
	n := 0
	for _, j := range t.jobs {
		if !j.restarting && !j.Ended() {
			n++
		}
	}
	return n
}

// trim forgets ended jobs until at most limit of them, pinned ones aside,
// are remembered: first the oldest the model was told about, else the
// oldest, so a job whose end has not been announced yet is not reported as
// lost while others can go first. A pinned job (see Manager.SetPinned) is
// neither forgotten nor counted. The Manager's lock must be held.
func (t *thread[H]) trim(limit int, pinned func(H) bool) {
	keep := func(id string) bool { return pinned != nil && pinned(t.jobs[id].Handle) }
	for {
		var candidates []string
		for _, id := range t.finished {
			if !keep(id) {
				candidates = append(candidates, id)
			}
		}
		if len(candidates) <= limit {
			return
		}
		i := slices.IndexFunc(candidates, func(id string) bool { return t.jobs[id].Notified() })
		id := candidates[max(i, 0)]
		delete(t.jobs, id)
		t.finished = slices.DeleteFunc(t.finished, func(f string) bool { return f == id })
	}
}

// Manager owns the jobs of every thread. It is shared by concurrent runs, so
// every field is guarded by mu.
type Manager[H Handle] struct {
	prefix string
	limits Limits

	mu sync.Mutex
	// pinned reports the ended jobs never forgotten; nil pins none.
	pinned   func(H) bool
	threads  map[string]*thread[H]
	closed   bool
	watchers sync.WaitGroup
	// starting tracks the starts in flight, so Close can wait for them.
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

// SetPinned makes the Manager keep every ended job whose handle pinned
// reports, such as one that waits for a decision: such a job is never
// forgotten to stay within Limits.MaxFinished, nor counted against it. pinned
// is called with the Manager's lock held, so it must be quick and must not
// call the Manager; a job it stops pinning is trimmed when the next job of
// its thread ends. nil pins none.
func (m *Manager[H]) SetPinned(pinned func(H) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pinned = pinned
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
// case it is skipped. A job committed after Close began is stopped at once
// and returned with Stopped set, so no work outlives Close.
//
// Start returns ErrShutdown after Close and a *LimitError when the thread
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
	go m.watch(t, j, j.episode)
	closed := m.closed
	m.mu.Unlock()
	if closed {
		ctx, cancel := detachedStop(context.Background())
		_, _ = requestStop(ctx, j) // a failed signal still ends in Done via the handle's own escalation
		cancel()
	}
	return j, nil
}

// Restart runs the ended job id of threadID again, as Start runs a new one:
// it reserves a slot of the thread's running limit (the job itself, having
// ended, does not count) and takes the job out of the finished list, then
// calls start without the lock. start makes the job's Handle run again: its
// Done must be open once start returns nil (the Handle stays the same value;
// a handle with episodes swaps its channel). start may block but must not
// call the Manager. On success the job is committed: stopped and notified
// are cleared and it is watched again, so Changed fires when it ends anew.
// When start fails (or panics) the reservation is rolled back and the job
// stays as it was, ended.
//
// Restart returns ErrShutdown after Close, ErrUnknownJob when the job is not
// held, is still running or is being restarted (or, after start, when
// StopAndForget dropped it meanwhile: the new run is then stopped and the
// job stays forgotten), and a *LimitError when the
// thread runs or is starting MaxRunning jobs, all without calling start; an
// error from start is returned as is. A job committed after Close began is
// stopped at once, as in Start.
func (m *Manager[H]) Restart(threadID, id string, start func() error) (*Job[H], error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrShutdown
	}
	t, ok := m.threads[threadID]
	var j *Job[H]
	if ok {
		j = t.jobs[id]
	}
	if j == nil || j.restarting || !j.Ended() {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrUnknownJob, id)
	}
	if t.running()+t.starting >= m.limits.MaxRunning {
		m.mu.Unlock()
		return nil, &LimitError{Limit: m.limits.MaxRunning}
	}
	// Out of the finished list, the job cannot be evicted while it restarts.
	at := slices.Index(t.finished, id)
	if at >= 0 {
		t.finished = slices.Delete(t.finished, at, at+1)
	}
	j.restarting = true
	j.episode++ // the ended episode's watcher, if still pending, stands down
	t.starting++
	m.starting.Add(1)
	m.mu.Unlock()
	committed := false
	defer func() {
		if !committed { // start failed or panicked: the job stays ended
			m.mu.Lock()
			t.starting--
			j.restarting = false
			if at >= 0 {
				t.finished = slices.Insert(t.finished, min(at, len(t.finished)), id)
				t.trim(m.limits.MaxFinished, m.pinned)
			} else if t.jobs[id] == j {
				// The ended episode's watcher stood down before recording
				// it: record it now, as it would have.
				m.recordEnd(t, j)
			}
			m.mu.Unlock()
		}
		m.starting.Done()
	}()

	if err := start(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	t.starting--
	committed = true
	j.restarting = false
	if m.threads[threadID] != t || t.jobs[id] != j {
		// StopAndForget dropped the job meanwhile (its run ended): the new
		// run has nobody left to report to, so it is stopped, not kept.
		if len(t.jobs) == 0 && t.starting == 0 && m.threads[threadID] == t {
			delete(m.threads, threadID)
		}
		m.mu.Unlock()
		ctx, cancel := detachedStop(context.Background())
		_, _ = requestStop(ctx, j) // a failed signal still ends in Done via the handle's own escalation
		cancel()
		return nil, fmt.Errorf("%w: %s was forgotten while it restarted", ErrUnknownJob, id)
	}
	j.stopped.Store(false)
	j.notified.Store(false)
	m.watchers.Add(1)
	go m.watch(t, j, j.episode)
	closed := m.closed
	m.mu.Unlock()
	if closed {
		ctx, cancel := detachedStop(context.Background())
		_, _ = requestStop(ctx, j) // a failed signal still ends in Done via the handle's own escalation
		cancel()
	}
	return j, nil
}

// Adopt registers h as the job id of threadID, an id handed out before (by
// this Manager before the thread was released, or by another process): the
// thread's numbering continues after it. A running h takes a slot of the
// running limit and is watched as a started job is; an ended h joins the
// finished list. started is the job's original start time.
//
// Adopt returns ErrShutdown after Close, a *LimitError when h runs and the
// thread is at its running limit, and an error when the thread already holds
// a job id.
func (m *Manager[H]) Adopt(threadID, id string, started time.Time, h H) (*Job[H], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrShutdown
	}
	t := m.thread(threadID)
	if _, ok := t.jobs[id]; ok {
		return nil, fmt.Errorf("jobs: thread %s already holds job %s", threadID, id)
	}
	j := &Job[H]{ID: id, ThreadID: threadID, Started: started, Handle: h}
	if j.Ended() {
		t.next = max(t.next, m.highestNumber([]string{id}))
		t.jobs[id] = j
		t.finished = append(t.finished, id)
		t.trim(m.limits.MaxFinished, m.pinned)
		return j, nil
	}
	if t.running()+t.starting >= m.limits.MaxRunning {
		if len(t.jobs) == 0 && t.starting == 0 {
			delete(m.threads, threadID)
		}
		return nil, &LimitError{Limit: m.limits.MaxRunning}
	}
	t.next = max(t.next, m.highestNumber([]string{id}))
	t.jobs[id] = j
	m.watchers.Add(1)
	go m.watch(t, j, j.episode)
	return j, nil
}

// AdoptStarting is Adopt for work that start begins now, such as a task
// taken over after a restart that resumes as it is adopted. Like Start, it
// first reserves a slot of the thread's running limit under the Manager's
// lock, so nothing is begun when the thread is full, then calls start
// without the lock (start may block but must not call the Manager), and
// registers the handle it returns as job id: watched as a started job is,
// or, when it already ended, in the finished list. When start fails (or
// panics) the reservation is rolled back. A job committed after Close began
// is stopped at once and returned, as in Start.
//
// AdoptStarting returns ErrShutdown after Close, a *LimitError when the
// thread runs or is starting MaxRunning jobs, and an error when the thread
// already holds job id, all without calling start; an error from start is
// returned as is. When another caller registered id while start ran, the
// new handle is stopped and an error returned.
func (m *Manager[H]) AdoptStarting(threadID, id string, started time.Time, start func() (H, error)) (*Job[H], error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrShutdown
	}
	t := m.thread(threadID)
	if _, ok := t.jobs[id]; ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("jobs: thread %s already holds job %s", threadID, id)
	}
	if t.running()+t.starting >= m.limits.MaxRunning {
		if len(t.jobs) == 0 && t.starting == 0 {
			delete(m.threads, threadID)
		}
		m.mu.Unlock()
		return nil, &LimitError{Limit: m.limits.MaxRunning}
	}
	t.starting++
	m.starting.Add(1)
	m.mu.Unlock()
	committed := false
	defer func() {
		if !committed { // start failed or panicked
			m.mu.Lock()
			t.starting--
			if len(t.jobs) == 0 && t.starting == 0 && m.threads[threadID] == t {
				delete(m.threads, threadID)
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
	if _, held := t.jobs[id]; held || m.threads[threadID] != t {
		m.mu.Unlock()
		ctx, cancel := detachedStop(context.Background())
		_ = h.Stop(ctx)
		cancel()
		return nil, fmt.Errorf("jobs: thread %s took job %s while it was adopted", threadID, id)
	}
	t.next = max(t.next, m.highestNumber([]string{id}))
	j := &Job[H]{ID: id, ThreadID: threadID, Started: started, Handle: h}
	t.jobs[id] = j
	if j.Ended() {
		t.finished = append(t.finished, id)
		t.trim(m.limits.MaxFinished, m.pinned)
	} else {
		m.watchers.Add(1)
		go m.watch(t, j, j.episode)
	}
	closed := m.closed
	m.mu.Unlock()
	if closed {
		ctx, cancel := detachedStop(context.Background())
		_, _ = requestStop(ctx, j) // a failed signal still ends in Done via the handle's own escalation
		cancel()
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

// watch waits for episode of j to end, then records its end (see
// recordEnd). A job already dropped (its nested run ended) needs nothing,
// and neither does one that Restart took again meanwhile (its episode
// changed): the job runs again, and its new watcher records its next end.
func (m *Manager[H]) watch(t *thread[H], j *Job[H], episode uint64) {
	defer m.watchers.Done()
	<-j.Handle.Done()
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.jobs[j.ID] != j || j.episode != episode {
		return
	}
	m.recordEnd(t, j)
}

// recordEnd adds the ended job j to t's finished list, forgets the oldest
// ended jobs beyond MaxFinished and wakes the thread's sleepers. The caller
// holds m.mu.
func (m *Manager[H]) recordEnd(t *thread[H], j *Job[H]) {
	t.finished = append(t.finished, j.ID)
	t.trim(m.limits.MaxFinished, m.pinned)
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

// All returns every job the Manager holds, of every thread, in no
// particular order.
func (m *Manager[H]) All() []*Job[H] {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []*Job[H]
	for _, t := range m.threads {
		for _, j := range t.jobs {
			list = append(list, j)
		}
	}
	return list
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

// requestStop marks j stopped and calls its Stop with ctx when it is still
// running, reporting whether it was. A job that already ended keeps its
// outcome.
func requestStop[H Handle](ctx context.Context, j *Job[H]) (bool, error) {
	if j.Ended() {
		return false, nil
	}
	j.stopped.Store(true)
	if err := j.Handle.Stop(ctx); err != nil {
		return true, fmt.Errorf("stop %s: %w", j.ID, err)
	}
	return true, nil
}

// Stop requests a stop and waits up to wait, or until ctx ends, for j to end.
func Stop[H Handle](ctx context.Context, j *Job[H], wait time.Duration) error {
	if j == nil {
		return errors.New("stop: no such job")
	}
	if _, err := requestStop(ctx, j); err != nil {
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

// stopAll stops jobs and waits for them all, sharing one wait deadline; it
// reports whether every job ended. The stop requests are sent concurrently,
// so a slow one (a container runtime's, say) holds back no other, and all of
// them have returned before the wait begins. Each runs under a detached ctx
// of its own (see detachedStop), which bounds it, since stopAll runs as a run
// or the Manager ends.
func stopAll[H Handle](ctx context.Context, list []*Job[H], wait time.Duration) bool {
	var sent sync.WaitGroup
	for _, j := range list {
		sent.Go(func() {
			stopCtx, cancel := detachedStop(ctx)
			defer cancel()
			_, _ = requestStop(stopCtx, j) // a failed signal still ends in Done via the handle's own escalation
		})
	}
	sent.Wait()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for _, j := range list {
		select {
		case <-j.Handle.Done():
			continue // already ended: never lose it to a fired timer
		default:
		}
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
// are dropped, so it can still read their outcome. The stop requests keep
// ctx's values but not its cancellation, so they go out even when ctx ended.
func (m *Manager[H]) StopAndForget(ctx context.Context, threadID string, ids []string, wait time.Duration, refresh func()) {
	var list []*Job[H]
	for _, id := range ids {
		if j := m.Get(threadID, id); j != nil {
			list = append(list, j)
		}
	}
	_ = stopAll(ctx, list, wait) // a job that outlives wait is still dropped: its run is over
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

// errNotEnded is returned by Close when some job had not ended by the end
// of its wait.
var errNotEnded = errors.New("jobs: some jobs did not end in time")

// Close refuses new jobs, stops every job and waits for them, up to wait, or
// until ctx's deadline when that comes first, and not at all when ctx is
// already done. It first waits, within the same wait, for the starts in
// flight: each one commits its job and stops it at once (see Start), so it is
// stopped and waited for with the others. A start still in flight when the
// wait passes stops its job as it commits, after Close returned. The entries
// are kept, so records refreshed afterwards show the jobs as stopped rather
// than lost. The stop requests keep ctx's values but not its cancellation.
// They are sent, concurrently, before the wait begins, and each may take up
// to stopRequestTimeout, so Close can return up to that much later than
// wait (or ctx's deadline).
//
// Close returns nil when every job ended in time; otherwise ctx.Err() when
// ctx is done, context.DeadlineExceeded when ctx's deadline cut the wait
// short, else errNotEnded. Calling it again is safe: it stops and waits for
// whatever is still running.
func (m *Manager[H]) Close(ctx context.Context, wait time.Duration) error {
	cut := false
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < wait {
			wait, cut = left, true
		}
	}
	if ctx.Err() != nil {
		wait = 0
	}
	if m.shutdown(ctx, max(wait, 0)) {
		return nil
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case cut:
		return context.DeadlineExceeded
	}
	return errNotEnded
}

// shutdown is Close's work; it reports whether every job and start ended
// within wait. ctx carries the values of the stop requests (see stopAll).
func (m *Manager[H]) shutdown(ctx context.Context, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	m.mu.Lock()
	m.closed = true
	inFlight := 0
	for _, t := range m.threads {
		inFlight += t.starting
	}
	m.mu.Unlock()
	// With no start in flight there is nothing to wait for; checking first
	// keeps a zero wait from racing the timer against a finished Wait.
	startsDone := inFlight == 0
	if !startsDone {
		started := make(chan struct{})
		go func() { m.starting.Wait(); close(started) }()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-started:
			startsDone = true
		case <-timer.C:
		}
		timer.Stop()
	}

	m.mu.Lock()
	var list []*Job[H]
	for _, t := range m.threads {
		for _, j := range t.jobs {
			list = append(list, j)
		}
	}
	m.mu.Unlock()
	// A start still in flight would Add a watcher while Wait runs, which
	// WaitGroup forbids, so the watchers are waited for only when none is.
	if stopAll(ctx, list, time.Until(deadline)) && startsDone {
		m.watchers.Wait()
		return true
	}
	return false
}
