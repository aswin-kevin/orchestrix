package orxdocker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/sandbox"
)

// killGrace is how long a stopped command's process group gets after
// SIGTERM before SIGKILL.
const killGrace = backend.DefaultKillGrace

// hardGrace is how long a command may run past its timeout before its
// container is removed: by then the in-container kill and the backstop have
// failed, which only a command that defeated them (it runs as the same user
// as the wrapper) can cause.
const hardGrace = 10 * time.Second

// exitWait bounds the wait for an exec to report its exit once its output
// stream ended.
const exitWait = 5 * time.Second

// Polling intervals for an exec's state while its output stream is open and
// after it closed.
const (
	pollOpen   = 250 * time.Millisecond
	pollClosed = 20 * time.Millisecond
)

// signalTimeout bounds one exec that signals a command.
const signalTimeout = 10 * time.Second

// stateDir holds the pid file of each running command (a tmpfs).
const stateDir = "/tmp"

// wrapper runs every command inside the container. Arguments: $1 token, $2
// backstop seconds, $3 working directory and $4 command, both base64: the
// Engine API carries argv as JSON strings, which cannot hold bytes that are
// not valid UTF-8 (file names, for instance).
//
// The command runs under setsid in a new session and process group. It is
// started as a background job of this non-interactive shell, so it is not a
// group leader and setsid does not fork: $! is the group id. The id goes to
// a pid file that Stop (and a timeout) uses to signal the whole group with a
// second exec, because cancelling an exec does not kill what it runs.
//
// The command's stderr joins its stdout, so the two keep their order.
//
// A backstop in its own group kills the command's group with SIGKILL after
// $2 seconds, even if the host process died or could not reach the daemon;
// it is killed as soon as the command ends. When the command's shell exits,
// whatever it left in its group gets SIGTERM and, after a grace period,
// SIGKILL, so nothing it spawned outlives it unless it left the group or
// killed this wrapper.
const wrapper = `f=` + stateDir + `/.orx-$1
d=$(printf '%s' "$3" | base64 -d; echo x)
c=$(printf '%s' "$4" | base64 -d; echo x)
cd -- "${d%x}" || exit 1
setsid /bin/sh -c "${c%x}" 2>&1 &
p=$!
echo "$p" > "$f"
setsid /bin/sh -c 'sleep "$1"; kill -KILL "-$2"' orx-backstop "$2" "$p" >/dev/null 2>&1 &
w=$!
wait "$p" 2>/dev/null
s=$?
kill -KILL "-$w" 2>/dev/null
if kill -0 "-$p" 2>/dev/null; then
	kill -TERM "-$p" 2>/dev/null
	i=0
	while [ "$i" -lt 20 ] && kill -0 "-$p" 2>/dev/null; do
		sleep 0.1 2>/dev/null || sleep 1
		i=$((i + 1))
	done
	kill -KILL "-$p" 2>/dev/null
fi
rm -f "$f"
exit "$s"
`

// signalScript sends signal $2 to the process group recorded under token
// $1. It exits 3 when no pid is recorded (not yet, or no longer).
const signalScript = `f=` + stateDir + `/.orx-$1
[ -s "$f" ] || exit 3
read -r p < "$f" || exit 3
kill -"$2" "-$p" 2>/dev/null
exit 0
`

// errEnded is signal's error when the command ended before a signal
// reached it.
var errEnded = errors.New("orxdocker: the command ended before it was signalled")

// exitNoPID is signalScript's status when no pid file exists.
const exitNoPID = 3

// errNoPID reports that a command's pid file never appeared or was removed
// (by the command itself, which runs as the same user).
var errNoPID = errors.New("orxdocker: the command's pid file is missing")

// Executor runs commands in one thread's container; it implements
// backend.Executor and backend.Starter. The container is created on the
// first command. Paths (ExecOptions.Dir) are paths inside the container;
// Manager.Backend wraps the executor in a sandbox.StarterBackend for file
// operations.
//
// Every command runs as "/bin/sh -c" in a new process group in the
// workspace (or ExecOptions.Dir), with only the image's environment,
// Config.Env and ExecOptions.Env, no stdin, stderr merged into stdout, and
// output capped at a head and a tail of MaxOutputBytes while it streams. The
// command and directory may hold any bytes (they travel base64-encoded);
// environment entries reach the daemon as JSON strings, so bytes that are
// not valid UTF-8 in them are replaced. A timeout or Stop sends
// SIGTERM to the group, then SIGKILL after backend.DefaultKillGrace; a
// backstop inside the container kills the group shortly after the timeout
// even if the host process is gone. Processes that leave the group (setsid)
// are not tracked, but cannot leave the container, which is the security
// boundary.
//
// The pid file and the backstop run as the command's user, so a command can
// defeat them. Execute and timeouts stay bounded whatever the command does:
// when the kill cannot end a stopped command within the grace periods, or a
// command still runs 10 seconds after its timeout, the container is
// force-removed, ending every command of the thread and discarding the
// workspace tmpfs, and the next command starts with a new container. The
// same happens, as a best effort, when a command's exec ends after its
// timeout or Stop without any signal having reached its group and with its
// wrapper dead of a signal (status 128 or more, or lost).
//
// A command ended by Stop, its timeout or a cancelled Execute reports
// ExitCode -1. A command killed by a signal from anything else (itself, or
// another process in the container) reports 128+n, the shell's status for
// signal n: orxdocker cannot tell that apart from a real exit status.
//
// Processes can still outlive their command inside the thread's container:
// a command can detach them (setsid, a double fork), or kill its own
// wrapper before its timeout, after which the exec ends and the command's
// group is no longer tracked. Such processes stay confined by the
// container's hardening (no network by default, dropped capabilities,
// no-new-privileges, process, memory and CPU limits) and end when the
// container is removed: Manager.Delete, Manager.ReapIdle, the idle TTL,
// Manager.Close, or a removal above.
type Executor struct {
	m      *Manager
	thread string
	sb     *sandbox.StarterBackend

	createMu sync.Mutex // serializes container creation

	mu       sync.Mutex // guards the fields below
	id       string     // container id; "" until created or after removal
	lastUsed time.Time
	active   int // commands started and not finished
}

var (
	_ backend.Executor = (*Executor)(nil)
	_ backend.Starter  = (*Executor)(nil)
)

// ContainerID returns the id of the thread's container, creating it (or
// reusing a running one of the thread with the same configuration) if
// needed.
func (e *Executor) ContainerID(ctx context.Context) (string, error) {
	e.acquire()
	defer e.release()
	return e.ensure(ctx)
}

// acquire marks a command as running, which keeps the idle janitor away.
func (e *Executor) acquire() {
	e.mu.Lock()
	e.active++
	e.lastUsed = time.Now()
	e.mu.Unlock()
}

func (e *Executor) release() {
	e.mu.Lock()
	e.active--
	e.lastUsed = time.Now()
	e.mu.Unlock()
}

// forget clears the container id if it is id (any id when id is "") and
// returns the cleared id, or "".
func (e *Executor) forget(id string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id == "" || (id != "" && e.id != id) {
		return ""
	}
	old := e.id
	e.id = ""
	return old
}

// uses reports whether the executor still runs commands in container id.
func (e *Executor) uses(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.id == id
}

// forgetIdle clears and returns the container id if no command is running
// and none ran since cutoff.
func (e *Executor) forgetIdle(cutoff time.Time) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id == "" || e.active > 0 || e.lastUsed.After(cutoff) {
		return ""
	}
	old := e.id
	e.id = ""
	return old
}

// usage returns when the executor was last used and whether a command is
// running.
func (e *Executor) usage() (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastUsed, e.active > 0
}

// evict clears and returns the container id unless a command is running.
func (e *Executor) evict() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active > 0 {
		return "", false
	}
	old := e.id
	e.id = ""
	return old, true
}

func (e *Executor) containerID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.id
}

// ensure returns the container id, creating or adopting the container.
func (e *Executor) ensure(ctx context.Context) (string, error) {
	if e.thread == "" {
		return "", ErrNoThread
	}
	e.createMu.Lock()
	defer e.createMu.Unlock()
	m := e.m
	if m.isClosed() {
		return "", ErrClosed
	}
	if id := e.containerID(); id != "" {
		return id, nil
	}
	id, err := e.adopt(ctx)
	if err != nil {
		return "", err
	}
	if id == "" {
		if id, err = m.c.createContainer(ctx, m.spec, e.thread); err != nil {
			return "", err
		}
	}
	err = m.track(e, id)
	if errors.Is(err, errRemoving) {
		// The adopted container started being removed meanwhile.
		if id, err = m.c.createContainer(ctx, m.spec, e.thread); err != nil {
			return "", err
		}
		err = m.track(e, id)
	}
	if err != nil {
		_ = m.c.removeContainer(context.WithoutCancel(ctx), id)
		return "", err
	}
	e.mu.Lock()
	e.id = id
	e.mu.Unlock()
	return id, nil
}

// adopt finds a running container of the thread with the same
// configuration, left by an earlier process or another Manager.
func (e *Executor) adopt(ctx context.Context) (string, error) {
	m := e.m
	list, err := m.c.listContainers(ctx, m.scopeFilters(
		LabelThreadHash+"="+threadHash(e.thread), LabelConfig+"="+m.spec.Labels[LabelConfig])...)
	if err != nil {
		return "", err
	}
	for _, s := range list {
		if s.State == "running" && !m.isRemoving(s.ID) {
			return s.ID, nil
		}
	}
	return "", nil
}

// Execute runs cmd and waits for it. When ctx ends first the command's group
// is stopped and ctx.Err() is returned with the output gathered so far.
func (e *Executor) Execute(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.ExecResult, error) {
	p, err := e.Start(ctx, cmd, opts)
	if err != nil {
		return backend.ExecResult{}, err
	}
	select {
	case <-p.Done():
		res, _ := p.Result()
		return res, nil
	case <-ctx.Done():
	}
	// ctx has ended, so the stop request gets a context of its own that
	// keeps ctx's values: with ctx itself the signal exec would fail at once.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signalTimeout)
	_ = p.Stop(stopCtx) // the timeout's backstop still ends the command if this fails
	cancel()
	select {
	case <-p.Done():
		res, _ := p.Result()
		return res, ctx.Err()
	case <-time.After(3 * e.m.killGrace):
		return backend.ExecResult{Output: p.Output(), ExitCode: -1}, ctx.Err()
	}
}

// Start runs cmd without waiting for it. ctx bounds only the start (creating
// the container on first use): the command ends on completion, timeout or
// Stop.
func (e *Executor) Start(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o := &e.m.cfg
	dir := opts.Dir
	if dir == "" {
		dir = o.WorkDir
	}
	if !path.IsAbs(dir) {
		return nil, fmt.Errorf("orxdocker: dir %q is not an absolute path", dir)
	}
	env, err := mergeEnv(o.Env, opts.Env)
	if err != nil {
		return nil, err
	}
	timeout := positiveOr(opts.Timeout, o.Timeout)
	maxOutput := positiveOr(opts.MaxOutputBytes, o.MaxOutputBytes)
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	backstop := int64((timeout+killGrace+time.Second)/time.Second) + 1

	e.acquire()
	p, err := e.start(ctx, token, []string{"/bin/sh", "-c", wrapper, "orx", token,
		strconv.FormatInt(backstop, 10), b64(dir), b64(cmd)}, env, timeout, maxOutput)
	if err != nil {
		e.release()
		return nil, err
	}
	return p, nil
}

// start creates and starts the exec and the goroutines that follow it.
func (e *Executor) start(ctx context.Context, token string, argv, env []string, timeout time.Duration, maxOutput int) (*process, error) {
	id, err := e.ensure(ctx)
	if err != nil {
		return nil, err
	}
	execID, err := e.createExec(ctx, id, argv, env)
	if err != nil {
		return nil, err
	}
	// ctx bounds the start only: the stream outlives it.
	body, err := e.m.c.execStart(ctx, execID)
	if err != nil {
		if ctx.Err() != nil {
			// The daemon may have started the command all the same.
			go func() { _ = e.signal(context.WithoutCancel(ctx), id, token, "KILL", nil) }()
			return nil, err
		}
		return nil, e.goneError(id, err)
	}
	p := &process{
		e:         e,
		container: id,
		token:     token,
		execID:    execID,
		out:       backend.NewOutputCapture(maxOutput),
		done:      make(chan struct{}),
		termSent:  make(chan struct{}),
		exited:    make(chan struct{}),
	}
	go p.follow(body, timeout)
	return p, nil
}

// createExec creates an exec in container id.
func (e *Executor) createExec(ctx context.Context, id string, argv, env []string) (string, error) {
	body := map[string]any{
		"AttachStdin":  false,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"Env":          env,
		"Cmd":          argv,
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := e.m.c.do(ctx, http.MethodPost, "/containers/"+id+"/exec", nil, body, &created); err != nil {
		return "", e.goneError(id, err)
	}
	return created.ID, nil
}

// goneError maps the daemon's answers for a removed (404) or stopped (409)
// container to ErrContainerGone and forgets the container, so the next
// command creates a new one.
func (e *Executor) goneError(id string, err error) error {
	if !isStatus(err, http.StatusNotFound, http.StatusConflict) {
		return err
	}
	if e.forget(id) != "" {
		e.m.forgetContainer(id)
	}
	return fmt.Errorf("%w: %w", ErrContainerGone, err)
}

// execState is the part of GET /exec/{id}/json orxdocker reads. ExitCode
// is null until the exec has exited, and Running is false before it starts
// as well as after it exited.
type execState struct {
	Running  bool `json:"Running"`
	ExitCode *int `json:"ExitCode"`
}

// exited returns the exit code once the exec has exited.
func (st execState) exited() (int, bool) {
	if st.Running || st.ExitCode == nil {
		return 0, false
	}
	return *st.ExitCode, true
}

func (e *Executor) inspectExec(ctx context.Context, execID string) (execState, error) {
	var st execState
	err := e.m.c.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(execID)+"/json", nil, nil, &st)
	return st, err
}

// signal sends sig ("TERM" or "KILL") to the process group of the command
// recorded under token in container id, through a short exec. It retries
// while the pid is not recorded and done is open, for up to about a second,
// then returns errNoPID; it returns errEnded when done closes first. ctx,
// further bounded by signalTimeout, bounds the whole request; once it ends
// signal returns at once.
func (e *Executor) signal(ctx context.Context, id, token, sig string, done <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(ctx, signalTimeout)
	defer cancel()
	var err error
	for range 10 {
		var code int
		code, err = e.runQuick(ctx, id, []string{"/bin/sh", "-c", signalScript, "orx-signal", token, sig})
		switch {
		case err == nil && code == 0:
			return nil
		case err == nil && code == exitNoPID:
			err = errNoPID
		case err == nil:
			err = fmt.Errorf("orxdocker: signal exec exited with status %d", code)
		}
		if ctx.Err() != nil {
			return err // every retry would fail the same way
		}
		select {
		case <-done:
			return errEnded
		case <-time.After(100 * time.Millisecond):
		}
	}
	return err
}

// runQuick runs a short internal command in container id to completion,
// discarding its output, and returns its exit status.
func (e *Executor) runQuick(ctx context.Context, id string, argv []string) (int, error) {
	if id == "" {
		return 0, ErrContainerGone
	}
	execID, err := e.createExec(ctx, id, argv, nil)
	if err != nil {
		return 0, err
	}
	body, err := e.m.c.execStart(ctx, execID)
	if err != nil {
		return 0, err
	}
	stop := context.AfterFunc(ctx, func() { _ = body.Close() })
	_ = demux(body, io.Discard)
	stop()
	_ = body.Close()
	deadline := time.Now().Add(e.m.exitWait)
	for {
		st, err := e.inspectExec(ctx, execID)
		if err != nil {
			return 0, err
		}
		if code, ok := st.exited(); ok {
			return code, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("orxdocker: internal exec still running %s after its output ended", e.m.exitWait)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(pollClosed):
		}
	}
}

// mergeEnv combines environment maps into KEY=value entries sorted by key, a
// later map's value winning over an earlier one's. A key that is empty or
// holds "=" or NUL is an error.
func mergeEnv(envs ...map[string]string) ([]string, error) {
	merged := map[string]string{}
	for _, env := range envs {
		for k, v := range env {
			if k == "" || strings.ContainsAny(k, "=\x00") {
				return nil, fmt.Errorf("orxdocker: env key %q is invalid", k)
			}
			merged[k] = v
		}
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(merged)) {
		out = append(out, k+"="+merged[k])
	}
	return out, nil
}

// b64 encodes a wrapper argument.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// newToken returns a random hex name for a command's pid file.
func newToken() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("orxdocker: token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// process is a started command; it implements backend.Process.
type process struct {
	e         *Executor
	container string // the container the command runs in
	token     string
	execID    string
	out       *backend.OutputCapture

	timedOut atomic.Bool
	stopOnce sync.Once
	stopErr  error
	// stopping is set when stop begins; termSent is closed once its
	// first signal has been sent or has failed, and unconfirmed records
	// that no signal reached the command's group.
	stopping    atomic.Bool
	termSent    chan struct{}
	unconfirmed atomic.Bool
	// exited is closed once the exec has reported its exit (or is gone).
	exited chan struct{}

	done   chan struct{}
	result backend.ExecResult // written before done is closed
}

var _ backend.Process = (*process)(nil)

// Done is closed once the command's shell has exited and its group has been
// cleaned up.
func (p *process) Done() <-chan struct{} { return p.done }

// Result returns the final result once Done is closed.
func (p *process) Result() (backend.ExecResult, bool) {
	select {
	case <-p.done:
		return p.result, true
	default:
		return backend.ExecResult{}, false
	}
}

// Output returns the output captured so far.
func (p *process) Output() string { return p.out.String() }

// Stop sends SIGTERM to the command's group and SIGKILL after
// backend.DefaultKillGrace if it is still running. When the signal cannot be
// sent, or the command still runs another grace period after SIGKILL, the
// container is force-removed, which ends every command of the thread.
// Stopping a finished command is a no-op.
//
// ctx bounds the exec that sends SIGTERM (and the SIGKILL that replaces it
// when SIGTERM cannot be sent). When ctx ends before the signal is sent,
// Stop returns that error but the command is still ended: SIGKILL follows
// after the grace period, and then the container's removal, as above. Only
// the first call signals; a later call returns nil, or the error of a
// removal that failed, never the first call's ended ctx.
func (p *process) Stop(ctx context.Context) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := p.stop(ctx); err != nil {
		return fmt.Errorf("orxdocker: stop: %w", err)
	}
	return nil
}

// stop acts only on its first call. A TERM that reaches no group (the pid
// file is missing, or the exec exited first) leaves the kill unconfirmed,
// which follow acts on (see confirmKill). ctx bounds the signals sent before
// stop returns; the escalation after it runs on its own. An error caused by
// ctx is returned by that call only: stopErr keeps the failures that no
// other ctx would have avoided.
func (p *process) stop(ctx context.Context) error {
	var ctxErr error
	p.stopOnce.Do(func() {
		p.stopping.Store(true)
		defer close(p.termSent)
		grace := p.e.m.killGrace
		notSent := func(err error) bool { return errors.Is(err, errNoPID) || errors.Is(err, errEnded) }
		err := p.e.signal(ctx, p.container, p.token, "TERM", p.exited)
		switch {
		case err == nil:
		case notSent(err):
			p.unconfirmed.Store(true)
		case ctx.Err() != nil:
			// The caller's ctx ended the request, not the container: the
			// escalation below still kills the command.
			ctxErr = err
		default:
			kerr := p.e.signal(ctx, p.container, p.token, "KILL", p.exited)
			switch {
			case kerr == nil:
			case notSent(kerr):
				p.unconfirmed.Store(true)
			case ctx.Err() != nil:
				ctxErr = errors.Join(err, kerr)
			default:
				// No signal can be sent: only removing the container is left.
				if rerr := p.remove(); rerr != nil {
					p.stopErr = errors.Join(err, kerr, rerr)
				}
				return
			}
		}
		// A missing pid file may only be late, so the kill gets its grace
		// periods before the container goes.
		go func() {
			if p.ended(grace) {
				return
			}
			_ = p.e.signal(context.Background(), p.container, p.token, "KILL", p.done)
			if p.ended(grace) {
				return
			}
			_ = p.remove() // the hard deadline retries
		}()
	})
	if ctxErr != nil {
		return ctxErr
	}
	return p.stopErr
}

// ended reports whether the command ends within d.
func (p *process) ended(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}

// remove force-removes the command's container, which kills everything in
// it, and forgets it so that the next command creates a new one.
func (p *process) remove() error {
	m := p.e.m
	m.mu.Lock()
	m.removing[p.container]++
	m.mu.Unlock()
	p.e.forget(p.container)
	m.forgetContainer(p.container)
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return m.finishRemoval(ctx, p.container)
}

// follow copies the output stream into the capture, waits for the exec to
// exit, and publishes the result. The stream can outlast the exec when a
// process that left the group holds it open, so the exec's state is polled
// and the stream is dropped killGrace after the exec exited. A command
// still running hardGrace after its timeout has its container removed.
func (p *process) follow(body io.ReadCloser, timeout time.Duration) {
	defer p.e.release()
	m := p.e.m
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_ = demux(body, p.out) // a broken stream ends the copy; the exec state decides
	}()
	timer := time.AfterFunc(timeout, func() {
		p.timedOut.Store(true)
		_ = p.stop(context.Background())
	})
	defer timer.Stop()
	var hardErr error // written before removed is closed
	removed := make(chan struct{})
	hard := time.AfterFunc(timeout+m.hardGrace, func() {
		p.timedOut.Store(true)
		hardErr = fmt.Errorf("orxdocker: the command still ran %s after its timeout; its container was removed", m.hardGrace)
		if err := p.remove(); err != nil {
			hardErr = fmt.Errorf("orxdocker: the command still ran %s after its timeout; removing its container: %w", m.hardGrace, err)
		}
		close(removed)
	})

	exitCode, err := p.waitExit(copied, removed)
	close(p.exited)
	hardFired := !hard.Stop()
	if hardFired { // whatever waitExit saw, the container is going
		<-removed
		err = hardErr
	} else if errors.Is(err, errStillRunning) {
		// Its status is lost: send SIGTERM (done closes right after, which
		// ends Stop's escalation; the backstop covers the rest).
		_ = p.stop(context.Background())
	}
	held := false
	select {
	case <-copied:
	case <-time.After(m.killGrace):
		held = true // something still holds the command's output
	}
	_ = body.Close()
	<-copied
	if !hardFired && (!timer.Stop() || p.stopping.Load()) {
		if cerr := p.confirmKill(held, exitCode, err); cerr != nil {
			err = cerr
		}
	}

	res := backend.ExecResult{Output: p.out.String(), Truncated: p.out.Truncated(), ExitCode: exitCode}
	if p.stopping.Load() && exitCode > 128 {
		// The wrapper reports a signal death as 128+n; after Stop or the
		// timeout that is the kill, which backend.ExecResult reports as -1.
		res.ExitCode = -1
	}
	if err != nil {
		res.ExitCode = -1
		res.Output += "\n[orxdocker: " + err.Error() + "]"
	}
	if p.timedOut.Load() {
		res.TimedOut = true
		res.ExitCode = -1
	}
	p.result = res
	close(p.done)
}

// confirmKill runs once the exec of a timed-out or stopped command has
// exited: unless the kill is confirmed, it removes the container, as the
// hard deadline would, and returns the error to report. The kill is
// unconfirmed when something still holds the command's output (held), or
// when no signal reached its group and the wrapper did not exit on its own
// (it died of a signal, status 128 and up, or its status is lost): the
// command, which runs as the same user, may have killed the wrapper and
// deleted its pid file.
func (p *process) confirmKill(held bool, exitCode int, err error) error {
	<-p.termSent // stop is running or about to: the timer fired or Stop was called
	wrapperKilled := err != nil || exitCode >= 128
	if !held && !(p.unconfirmed.Load() && wrapperKilled) {
		return nil
	}
	if !p.e.uses(p.container) {
		return nil // already removed (by Stop's escalation, say)
	}
	if rerr := p.remove(); rerr != nil {
		return fmt.Errorf("orxdocker: the command's kill could not be confirmed after it was stopped; removing its container: %w", rerr)
	}
	return errors.New("orxdocker: the command's kill could not be confirmed after it was stopped; its container was removed")
}

// errStillRunning is waitExit's error for an exec that does not report its
// exit after its output ended.
var errStillRunning = errors.New("orxdocker: exec still running")

// waitExit polls the exec until it has exited and returns its exit code. An
// error means the state could not be read (for instance the container was
// removed), the exec still ran exitWait after its output ended, or removed
// was closed.
func (p *process) waitExit(copied, removed <-chan struct{}) (int, error) {
	exitWait := p.e.m.exitWait
	var deadline time.Time // set once the stream ended
	failures := 0
	for {
		poll := pollClosed
		if copied != nil {
			poll = pollOpen
		}
		select {
		case <-copied: // nil once closed
			copied = nil // the stream ended: the exec is about to report its exit
			deadline = time.Now().Add(exitWait)
		case <-time.After(poll):
		case <-removed:
			return -1, errors.New("orxdocker: container removed")
		}
		ctx, cancel := context.WithTimeout(context.Background(), signalTimeout)
		st, err := p.e.inspectExec(ctx, p.execID)
		cancel()
		switch {
		case err == nil:
			if code, ok := st.exited(); ok {
				return code, nil
			}
			if copied == nil && time.Now().After(deadline) {
				return -1, fmt.Errorf("%w %s after its output ended", errStillRunning, exitWait)
			}
			failures = 0
		default:
			failures++
			if isStatus(err, http.StatusNotFound) || failures >= 20 {
				return -1, err
			}
		}
	}
}
