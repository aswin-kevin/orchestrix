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
)

// cleanupTimeout bounds the API calls of Close and of the idle janitor.
const cleanupTimeout = 30 * time.Second

// Manager runs one container per thread. Its Backend method fits
// orchestrix.WithBackendFactory; each thread's container is created on its
// first command, reused by later ones (and by later runs of the thread),
// and removed after Options.IdleTTL without commands, by Delete, or by
// Close. A Manager is safe for concurrent use.
//
// A container still running when the process starts again is reused for its
// thread if its configuration (LabelConfig) matches; Reap removes the ones
// nobody will use.
type Manager struct {
	opts Options
	spec containerConfig
	c    *client

	mu      sync.Mutex
	closed  bool
	threads map[string]*Executor   // the executor Executor and Backend return
	live    map[*Executor]struct{} // executors that may own a container

	stop        chan struct{}
	janitorDone chan struct{}
}

// NewManager validates opts and returns a Manager. It does not contact the
// daemon (see Ping); when Options.IdleTTL is positive it starts a goroutine
// that removes idle containers until Close.
func NewManager(opts Options) (*Manager, error) {
	o, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	if _, err := sandbox.New(nopExec{}, sandbox.Options{Root: o.WorkDir, OpTimeout: o.OpTimeout}); err != nil {
		return nil, fmt.Errorf("orxdocker: %w", err)
	}
	m := &Manager{
		opts:        o,
		spec:        o.containerSpec(),
		c:           newClient(o.Socket, o.APIVersion),
		threads:     map[string]*Executor{},
		live:        map[*Executor]struct{}{},
		stop:        make(chan struct{}),
		janitorDone: make(chan struct{}),
	}
	if o.IdleTTL > 0 {
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

// Ping checks that the daemon answers on the configured socket.
func (m *Manager) Ping(ctx context.Context) error {
	return m.c.do(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// Backend returns the sandboxed backend of a thread: a sandbox.StarterBackend
// rooted at Options.WorkDir whose commands run in the thread's container. It
// implements backend.Backend, backend.MultiEditor, backend.Executor and
// backend.Starter, so it works with orchestrix.WithTasks. Pass the method to
// orchestrix.WithBackendFactory.
func (m *Manager) Backend(threadID string) backend.Backend {
	return m.Executor(threadID).sb
}

// Executor returns the thread's executor, whose paths (ExecOptions.Dir) are
// paths inside the container. Most callers want Backend.
func (m *Manager) Executor(threadID string) *Executor {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.threads[threadID]; ok {
		return e
	}
	e := &Executor{m: m, thread: threadID}
	sb, err := sandbox.NewStarter(e, sandbox.Options{Root: m.opts.WorkDir, OpTimeout: m.opts.OpTimeout})
	if err != nil {
		panic("orxdocker: sandbox options checked by NewManager: " + err.Error()) // unreachable
	}
	e.sb = sb
	m.threads[threadID] = e
	return e
}

// track records that e owns a container. It reports false once the Manager
// is closed; the caller then removes the container.
func (m *Manager) track(e *Executor) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.live[e] = struct{}{}
	return true
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
	return append([]string{LabelManaged + "=true", LabelScope + "=" + m.opts.Scope}, extra...)
}

// ReapIdle removes the containers of this Manager's threads that have run
// no command for Options.IdleTTL and have none running, and reports how
// many it removed. The janitor calls it periodically; with a negative
// IdleTTL it removes nothing.
func (m *Manager) ReapIdle(ctx context.Context) (int, error) {
	if m.opts.IdleTTL <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-m.opts.IdleTTL)
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
// executors created or reused, killing their commands. Commands fail with
// ErrClosed afterwards. Containers of other processes are left alone.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	execs := make([]*Executor, 0, len(m.live))
	for e := range m.live {
		execs = append(execs, e)
	}
	m.live = map[*Executor]struct{}{}
	m.mu.Unlock()
	close(m.stop)
	<-m.janitorDone
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	var errs []error
	for _, e := range execs {
		if id := e.forget(""); id != "" {
			errs = append(errs, m.c.removeContainer(ctx, id))
		}
	}
	return errors.Join(errs...)
}

// janitor calls ReapIdle until Close.
func (m *Manager) janitor() {
	defer close(m.janitorDone)
	every := min(max(m.opts.IdleTTL/4, time.Second), time.Minute)
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
