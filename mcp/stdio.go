package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aswin-kevin/orchestrix/internal/hostenv"
	"github.com/aswin-kevin/orchestrix/internal/procgroup"
)

// readBufferBytes is the stdout read buffer; longer lines are assembled from
// several reads, up to MaxMessageBytes.
const readBufferBytes = 64 << 10

// StdioConfig describes an MCP server run as a child process speaking
// newline-delimited JSON-RPC on its stdin and stdout.
type StdioConfig struct {
	// Command is the executable to run (looked up in PATH when bare).
	Command string
	// Args are its arguments.
	Args []string
	// Dir is its working directory; empty → the current one.
	Dir string
	// Env holds variables added last, overriding inherited ones; nil → none.
	// A key that is empty or holds '=' or NUL is an error.
	Env map[string]string
	// InheritEnv passes the whole host environment instead of only a small
	// allowlist (PATH, HOME, USER, LOGNAME, SHELL, LANG, LC_ALL, TMPDIR and
	// TZ, the shell backend's default list). It may expose secrets such as
	// API keys to the server.
	InheritEnv bool
	// MaxMessageBytes caps one message (line) from the server; a longer one
	// ends the transport. 0 → DefaultMaxMessageBytes; negative is an
	// error.
	MaxMessageBytes int
	// CloseGrace is how long each step of Close waits, how long leftovers of
	// an exited server get after SIGTERM, and how long a crash report waits
	// for the exit status; 0 → DefaultCloseGrace; negative is an error.
	CloseGrace time.Duration
}

// stdioTransport runs a server process; it implements Transport.
type stdioTransport struct {
	cmd      *exec.Cmd
	stdin    *os.File // our write end of the server's stdin
	stdout   *os.File // our read end of the server's stdout
	stderr   *os.File // our read end of the server's stderr
	tail     *ring    // the end of stderr
	maxBytes int
	grace    time.Duration

	msgs       chan json.RawMessage
	writes     chan writeRequest
	exited     chan struct{} // closed once the server process has been waited for
	waitErr    error         // written before exited is closed
	reaped     chan struct{} // closed once the server's leftovers have been reaped
	stderrDone chan struct{}
	// groupGone is set once the process group has been seen empty after the
	// server was waited for. From then on its id may belong to unrelated
	// processes, so nothing signals it any more.
	groupGone atomic.Bool

	errMu sync.Mutex
	err   error

	closing chan struct{}
	rel     release
	wg      sync.WaitGroup
}

// writeRequest is one message for the writer goroutine; the write's error is
// sent on done (buffered).
type writeRequest struct {
	line []byte
	done chan error
}

// StartStdio starts the server process. On unix it leads its own process
// group, so Close also stops everything it spawned, and whatever the server
// leaves running when it exits on its own is killed then; elsewhere only the
// process itself is killed.
//
// When the server's stdout ends, Messages closes only once the server has
// exited and its stderr is drained, or after CloseGrace, so Err carries the
// exit status and stderr tail; pending requests therefore fail up to
// CloseGrace after a crash.
func StartStdio(cfg StdioConfig) (Transport, error) {
	if cfg.Command == "" {
		return nil, errors.New("mcp: stdio: missing command")
	}
	if cfg.MaxMessageBytes < 0 {
		return nil, errors.New("mcp: MaxMessageBytes is negative")
	}
	if cfg.CloseGrace < 0 {
		return nil, errors.New("mcp: CloseGrace is negative")
	}
	if key, bad := hostenv.InvalidKey(cfg.Env); bad {
		return nil, fmt.Errorf("mcp: env key %q is invalid", key)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = environ(cfg)
	procgroup.Setup(cmd)

	var p pipes
	if err := p.open(); err != nil {
		return nil, fmt.Errorf("mcp: stdio: %w", err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.child[0], p.child[1], p.child[2]
	startErr := cmd.Start()
	p.closeChild() // the server holds its own copies
	if startErr != nil {
		p.closeOurs()
		return nil, fmt.Errorf("mcp: stdio: start %s: %w", cfg.Command, startErr)
	}

	t := &stdioTransport{
		cmd:        cmd,
		stdin:      p.ours[0],
		stdout:     p.ours[1],
		stderr:     p.ours[2],
		tail:       newRing(DefaultStderrBytes),
		maxBytes:   positiveOr(cfg.MaxMessageBytes, DefaultMaxMessageBytes),
		grace:      positiveOr(cfg.CloseGrace, DefaultCloseGrace),
		msgs:       make(chan json.RawMessage),
		writes:     make(chan writeRequest),
		exited:     make(chan struct{}),
		reaped:     make(chan struct{}),
		stderrDone: make(chan struct{}),
		closing:    make(chan struct{}),
	}
	t.wg.Add(4)
	go t.waitProcess()
	go t.drainStderr()
	go t.writeLoop()
	go t.readLoop()
	return t, nil
}

// environ builds the server's environment: the allowlist or the whole host
// environment, then cfg.Env in key order. exec.Cmd keeps the last value of a
// repeated key, so cfg.Env overrides.
func environ(cfg StdioConfig) []string {
	env := hostenv.Inherited(hostenv.DefaultNames(), cfg.InheritEnv)
	return append(env, hostenv.Pairs(cfg.Env)...)
}

// Send writes msg as one line to the server's stdin.
func (t *stdioTransport) Send(ctx context.Context, msg json.RawMessage) error {
	line := make([]byte, 0, len(msg)+1)
	line = append(append(line, msg...), '\n')
	req := writeRequest{line: line, done: make(chan error, 1)}
	select {
	case t.writes <- req:
	case <-t.closing:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.done:
		if err != nil {
			return fmt.Errorf("mcp: stdio: write: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err() // the line is still written whole, later
	}
}

// Messages yields the server's messages.
func (t *stdioTransport) Messages() <-chan json.RawMessage { return t.msgs }

// Err reports why the transport ended, with the tail of the server's stderr.
func (t *stdioTransport) Err() error {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return t.err
}

// Close stops the server: it closes stdin, then after CloseGrace sends
// SIGTERM to the process group, then after another CloseGrace SIGKILL, and
// waits for the process, its leftovers (up to two more CloseGrace periods)
// and the transport's goroutines. See Transport for the contract; the
// escalation keeps going in the background when ctx ends first.
func (t *stdioTransport) Close(ctx context.Context) error {
	t.rel.start(func() error {
		close(t.closing)
		err := t.stop()
		<-t.exited
		// A process that left the group could hold the pipes open forever.
		_ = t.stdout.Close()
		_ = t.stderr.Close()
		t.wg.Wait()
		return err
	})
	return t.rel.wait(ctx)
}

// stop escalates until the server has exited and its leftovers are reaped.
// Nothing is signalled once groupGone is set: by then the server has been
// waited for and its group seen empty, so the id may belong to someone else.
func (t *stdioTransport) stop() error {
	_ = t.stdin.Close()
	if t.awaitReaped() || t.groupGone.Load() {
		return nil
	}
	if err := procgroup.Terminate(t.cmd.Process); err != nil {
		return fmt.Errorf("mcp: stdio: terminate: %w", err)
	}
	if t.awaitReaped() || t.groupGone.Load() {
		return nil
	}
	err := procgroup.Kill(t.cmd.Process)
	// The server may have left its group (setpgid); kill it directly too so
	// Close can wait for it. Its pid stays ours until it has been waited for,
	// and Kill does nothing once it has.
	if killErr := t.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) && err == nil {
		err = killErr
	}
	t.awaitReaped()
	if err != nil {
		return fmt.Errorf("mcp: stdio: kill: %w", err)
	}
	return nil
}

// awaitReaped waits up to CloseGrace for the server to exit and its
// leftovers to be reaped, reporting whether that happened.
func (t *stdioTransport) awaitReaped() bool {
	timer := time.NewTimer(t.grace)
	defer timer.Stop()
	select {
	case <-t.reaped:
		return true
	case <-timer.C:
		return false
	}
}

// waitProcess waits for the server, then kills whatever it left running in
// its group, whether the server exited on its own or was stopped, and latches
// groupGone once the group is seen empty.
func (t *stdioTransport) waitProcess() {
	defer t.wg.Done()
	t.waitErr = t.cmd.Wait()
	close(t.exited)
	if procgroup.Reap(t.cmd.Process, t.grace) {
		t.groupGone.Store(true)
	}
	close(t.reaped)
}

func (t *stdioTransport) drainStderr() {
	defer t.wg.Done()
	defer close(t.stderrDone)
	_, _ = io.Copy(t.tail, t.stderr) // ring never fails; EOF or Close ends the copy
}

func (t *stdioTransport) writeLoop() {
	defer t.wg.Done()
	for {
		select {
		case req := <-t.writes:
			_, err := t.stdin.Write(req.line)
			req.done <- err
		case <-t.closing:
			return
		}
	}
}

// readLoop delivers the server's stdout line by line, skipping blank lines
// and lines that are not JSON objects (servers that log to stdout), then
// records why it ended and closes Messages.
func (t *stdioTransport) readLoop() {
	defer t.wg.Done()
	defer close(t.msgs)
	br := bufio.NewReaderSize(t.stdout, readBufferBytes)
	for {
		line, err := readLine(br, t.maxBytes)
		if err != nil {
			t.end(err)
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		select {
		case t.msgs <- json.RawMessage(line):
		case <-t.closing:
			return
		}
	}
}

// end records why the transport ended. After EOF it waits up to CloseGrace
// for the server's exit and last stderr output, so a crash is reported with
// its exit status and message.
func (t *stdioTransport) end(readErr error) {
	select {
	case <-t.closing:
		return // closed by us: nothing to report
	default:
	}
	var err error
	if errors.Is(readErr, io.EOF) {
		err = t.exitError()
	} else {
		err = fmt.Errorf("mcp: stdio: %w", readErr)
	}
	if tail := strings.TrimSpace(t.tail.String()); tail != "" {
		err = fmt.Errorf("%w; stderr: %s", err, tail)
	}
	t.errMu.Lock()
	t.err = err
	t.errMu.Unlock()
}

// exitError describes a server whose stdout reached EOF.
func (t *stdioTransport) exitError() error {
	timer := time.NewTimer(t.grace)
	defer timer.Stop()
	for _, done := range []chan struct{}{t.stderrDone, t.exited} {
		select {
		case <-done:
		case <-timer.C:
			return errors.New("mcp: stdio: server closed its stdout")
		case <-t.closing:
			return errors.New("mcp: stdio: server closed its stdout")
		}
	}
	if t.waitErr != nil {
		return fmt.Errorf("mcp: stdio: server exited: %w", t.waitErr)
	}
	return errors.New("mcp: stdio: server exited")
}

// errLineTooLong reports a message over the size cap.
var errLineTooLong = errors.New("message too long")

// readLine returns the next line without its newline. A line longer than max
// bytes is an error; a final line without a newline is returned before EOF.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > max+1 { // +1 for the newline
			return nil, fmt.Errorf("%w: over %d bytes", errLineTooLong, max)
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, nil
		default:
			return nil, err
		}
	}
}

// pipes are the three standard streams of a child: child ends go to the
// process, ours stay with the transport.
type pipes struct {
	child [3]*os.File // stdin read end, stdout and stderr write ends
	ours  [3]*os.File // stdin write end, stdout and stderr read ends
}

func (p *pipes) open() error {
	for i := range 3 {
		r, w, err := os.Pipe()
		if err != nil {
			p.closeChild()
			p.closeOurs()
			return err
		}
		if i == 0 {
			p.child[i], p.ours[i] = r, w
		} else {
			p.child[i], p.ours[i] = w, r
		}
	}
	return nil
}

func (p *pipes) closeChild() { closeAll(p.child[:]) }
func (p *pipes) closeOurs()  { closeAll(p.ours[:]) }

func closeAll(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// ring keeps the last size bytes written to it.
type ring struct {
	mu   sync.Mutex
	size int
	buf  []byte
}

func newRing(size int) *ring { return &ring{size: size} }

// Write implements io.Writer; it never fails.
func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.size; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// positiveOr returns v, or def when v is not positive.
func positiveOr[T int | time.Duration](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}
