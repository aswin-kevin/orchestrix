package orxdocker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/sandbox"
)

// Errors returned by a Manager and its executors.
var (
	// ErrClosed is returned once the Manager has been closed.
	ErrClosed = errors.New("orxdocker: manager closed")
	// ErrContainerGone is returned when a thread's container was removed or
	// stopped behind the Manager's back. The next command creates a new,
	// empty container.
	ErrContainerGone = errors.New("orxdocker: container is gone")
	// errRemoving is track's error for a container being removed.
	errRemoving = errors.New("orxdocker: container is being removed")
	// ErrNoThread is returned by an executor of the empty thread id. Every
	// run needs its own Input.ThreadID: the thread id names the container,
	// so an empty one would put unrelated runs in one container.
	ErrNoThread = errors.New("orxdocker: empty thread id: every run needs its own Input.ThreadID")
)

// cleanupTimeout bounds the API calls of the idle janitor and of the
// background removal of evicted containers.
const cleanupTimeout = 30 * time.Second

// Manager runs one container per thread. Its Backend method fits
// orchestrix.WithBackendFactory; each thread's container is created on its
// first command, reused by later ones (and by later runs of the thread),
// and removed after Config.IdleTTL without commands, by Delete, or by
// Close. A Manager is safe for concurrent use.
//
// A container still running when the process starts again is reused for its
// thread if its configuration (LabelConfig) matches; Reap removes the ones
// nobody will use.
type Manager struct {
	cfg  Config
	spec containerConfig
	c    *client

	mu     sync.Mutex
	closed bool
	// closeDone is closed once the first Close has finished; closeErr,
	// written before, is its result.
	closeDone chan struct{}
	closeErr  error
	threads   map[string]*Executor   // the executor Executor and Backend return
	live      map[*Executor]struct{} // executors that may own a container
	// removing counts the removals in flight per container id; adopt and
	// track skip those containers.
	removing  map[string]int
	evictions sync.WaitGroup // background removals of evicted containers

	stop        chan struct{}
	janitorDone chan struct{}

	// Waits, changed only by tests.
	killGrace time.Duration // between SIGTERM, SIGKILL and removing the container
	hardGrace time.Duration // after a command's timeout before its container is removed
	exitWait  time.Duration // for an exec to report its exit once its output ended
}

// NewManager validates cfg and returns a Manager. It does not contact the
// daemon (see Ping); unless Config.IdleTTL is Unlimited it starts a
// goroutine that removes idle containers until Close.
func NewManager(cfg Config) (*Manager, error) {
	c, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if _, err := sandbox.New(nopExec{}, sandbox.Config{Root: c.WorkDir, OpTimeout: c.OpTimeout}); err != nil {
		return nil, fmt.Errorf("orxdocker: %w", err)
	}
	m := &Manager{
		cfg:         c,
		spec:        c.containerSpec(),
		c:           newClient(c.Socket, c.Dial, c.APIVersion),
		closeDone:   make(chan struct{}),
		threads:     map[string]*Executor{},
		live:        map[*Executor]struct{}{},
		removing:    map[string]int{},
		stop:        make(chan struct{}),
		janitorDone: make(chan struct{}),
		killGrace:   killGrace,
		hardGrace:   hardGrace,
		exitWait:    exitWait,
	}
	if c.IdleTTL > 0 {
		go m.janitor()
	} else {
		close(m.janitorDone)
	}
	return m, nil
}

// nopExec lets NewManager check the sandbox options up front.
type nopExec struct{}

func (nopExec) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	return backend.ExecResult{}, errors.ErrUnsupported
}

// Ping checks that the daemon answers on the configured socket or Dial.
func (m *Manager) Ping(ctx context.Context) error {
	return m.c.do(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// Backend returns the sandboxed backend of a thread: a sandbox.StarterBackend
// rooted at Config.WorkDir whose commands run in the thread's container. It
// implements backend.Backend, backend.MultiEditor, backend.Executor and
// backend.Starter, so it works with orchestrix.WithTasks. Pass the method to
// orchestrix.WithBackendFactory. The empty thread id is rejected with
// ErrNoThread when a command runs: give every run an Input.ThreadID.
func (m *Manager) Backend(threadID string) backend.Backend {
	return m.Executor(threadID).sb
}

// Executor returns the thread's executor, whose paths (ExecOptions.Dir) are
// paths inside the container. Most callers want Backend. Creating one past
// Config.MaxThreads evicts the least recently used executor that runs no
// command and removes its container in the background.
func (m *Manager) Executor(threadID string) *Executor {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.threads[threadID]; ok {
		return e
	}
	e := &Executor{m: m, thread: threadID, lastUsed: time.Now()}
	sb, err := sandbox.NewStarter(e, sandbox.Config{Root: m.cfg.WorkDir, OpTimeout: m.cfg.OpTimeout})
	if err != nil {
		panic("orxdocker: sandbox options checked by NewManager: " + err.Error()) // unreachable
	}
	e.sb = sb
	if m.closed {
		return e // its commands fail with ErrClosed; nothing to keep or evict
	}
	if len(m.threads) >= m.cfg.MaxThreads {
		if id := m.evictLocked(); id != "" {
			m.removing[id]++
			m.evictions.Add(1)
			go func() {
				defer m.evictions.Done()
				ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
				defer cancel()
				_ = m.finishRemoval(ctx, id) // a failure leaves it to Reap
			}()
		}
	}
	m.threads[threadID] = e
	return e
}

// finishRemoval removes container id, which the caller marked in
// m.removing, and clears the mark.
func (m *Manager) finishRemoval(ctx context.Context, id string) error {
	err := m.c.removeContainer(ctx, id)
	m.mu.Lock()
	if m.removing[id]--; m.removing[id] <= 0 {
		delete(m.removing, id)
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) isRemoving(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.removing[id] > 0
}

// evictLocked drops the least recently used executor that runs no command
// from threads and returns its container id, or "" when it has none. When
// every executor is busy it evicts nothing and the cap is exceeded. m.mu is
// held.
func (m *Manager) evictLocked() string {
	var lru *Executor
	var lruUsed time.Time
	for _, e := range m.threads {
		used, busy := e.usage()
		if !busy && (lru == nil || used.Before(lruUsed)) {
			lru, lruUsed = e, used
		}
	}
	if lru == nil {
		return ""
	}
	id, ok := lru.evict()
	if !ok { // it started a command meanwhile
		return ""
	}
	delete(m.threads, lru.thread)
	delete(m.live, lru)
	return id
}

// track records that e owns container id. It fails with ErrClosed once
// the Manager is closed (the caller then removes the container) and with
// errRemoving while id is being removed.
func (m *Manager) track(e *Executor, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if m.removing[id] > 0 {
		return errRemoving
	}
	m.live[e] = struct{}{}
	return nil
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Delete removes the thread's container, with every file in its workspace,
// and stops its commands. That includes containers of the thread that
// another process using the same scope created. A later command of the
// thread starts with a new container.
func (m *Manager) Delete(ctx context.Context, threadID string) error {
	m.mu.Lock()
	var execs []*Executor
	for e := range m.live {
		if e.thread == threadID {
			execs = append(execs, e)
			delete(m.live, e)
		}
	}
	delete(m.threads, threadID)
	m.mu.Unlock()
	var errs []error
	for _, e := range execs {
		if id := e.forget(""); id != "" {
			errs = append(errs, m.c.removeContainer(ctx, id))
		}
	}
	list, err := m.c.listContainers(ctx, m.scopeFilters(LabelThreadHash+"="+threadHash(threadID))...)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, s := range list {
		errs = append(errs, m.c.removeContainer(ctx, s.ID))
	}
	return errors.Join(errs...)
}

// scopeFilters returns the label filters selecting this Manager's
// containers, plus extra.
func (m *Manager) scopeFilters(extra ...string) []string {
	return append([]string{LabelManaged + "=true", LabelScope + "=" + m.cfg.Scope}, extra...)
}

// ReapIdle removes the containers of this Manager's threads that have run
// no command for Config.IdleTTL and have none running, and reports how
// many it removed. The janitor calls it periodically; with IdleTTL
// Unlimited it removes nothing.
func (m *Manager) ReapIdle(ctx context.Context) (int, error) {
	if m.cfg.IdleTTL <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-m.cfg.IdleTTL)
	m.mu.Lock()
	var idle []string
	for e := range m.live {
		if id := e.forgetIdle(cutoff); id != "" {
			idle = append(idle, id)
			delete(m.live, e)
			if m.threads[e.thread] == e {
				delete(m.threads, e.thread)
			}
		}
	}
	m.mu.Unlock()
	var errs []error
	for _, id := range idle {
		errs = append(errs, m.c.removeContainer(ctx, id))
	}
	return len(idle), errors.Join(errs...)
}

// ReapOptions selects the containers Reap removes.
type ReapOptions struct {
	// OlderThan limits Reap to containers created at least this long ago;
	// 0 means any age.
	OlderThan time.Duration
	// Keep reports the threads whose containers must stay, for instance
	// those a checkpoint store still has. nil keeps none.
	Keep func(threadID string) bool
}

// Reap removes containers of the Manager's scope, running or not, whoever
// created them, that opts selects, and returns their ids. Use it at startup
// or from a periodic job to clear containers orphaned by a crashed process
// or left behind for deleted threads. Commands running in a removed
// container are killed.
func (m *Manager) Reap(ctx context.Context, opts ReapOptions) ([]string, error) {
	list, err := m.c.listContainers(ctx, m.scopeFilters()...)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-opts.OlderThan)
	var removed []string
	var errs []error
	for _, s := range list {
		if opts.OlderThan > 0 && time.Unix(s.Created, 0).After(cutoff) {
			continue
		}
		if opts.Keep != nil && opts.Keep(s.Labels[LabelThread]) {
			continue
		}
		m.forgetContainer(s.ID)
		if err := m.c.removeContainer(ctx, s.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, s.ID)
	}
	return removed, errors.Join(errs...)
}

// forgetContainer drops a container about to be removed from the executors
// that use it.
func (m *Manager) forgetContainer(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for e := range m.live {
		if e.forget(id) != "" {
			delete(m.live, e)
		}
	}
}

// Close stops the janitor and removes every container this Manager's
// executors created or reused, killing their commands, then waits for the
// removal of evicted containers. ctx bounds all of it. Commands fail with
// ErrClosed afterwards. Containers of other processes are left alone.
//
// Only the first call does the work. Later and concurrent calls wait for it
// to finish, or for their own ctx to end, and return its result.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		select {
		case <-m.closeDone:
			return m.closeErr
		case <-ctx.Done():
			return fmt.Errorf("orxdocker: close: %w", ctx.Err())
		}
	}
	m.closed = true
	execs := make([]*Executor, 0, len(m.live))
	for e := range m.live {
		execs = append(execs, e)
	}
	m.live = map[*Executor]struct{}{}
	m.mu.Unlock()
	m.closeErr = m.close(ctx, execs)
	close(m.closeDone)
	return m.closeErr
}

// close does the work of the first Close.
func (m *Manager) close(ctx context.Context, execs []*Executor) error {
	var errs []error
	close(m.stop)
	select {
	case <-m.janitorDone:
	case <-ctx.Done(): // its removals are detached and bounded
		errs = append(errs, fmt.Errorf("orxdocker: stopping the janitor: %w", ctx.Err()))
	}
	for _, e := range execs {
		if id := e.forget(""); id != "" {
			errs = append(errs, m.c.removeContainer(ctx, id))
		}
	}
	// No eviction starts once closed is set: wait for those in flight.
	evicted := make(chan struct{})
	go func() {
		m.evictions.Wait()
		close(evicted)
	}()
	select {
	case <-evicted:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("orxdocker: removing evicted containers: %w", ctx.Err()))
	}
	return errors.Join(errs...)
}

// janitor calls ReapIdle until Close.
func (m *Manager) janitor() {
	defer close(m.janitorDone)
	every := min(max(m.cfg.IdleTTL/4, time.Second), time.Minute)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			_, _ = m.ReapIdle(ctx) // retried on the next tick
			cancel()
		}
	}
}
