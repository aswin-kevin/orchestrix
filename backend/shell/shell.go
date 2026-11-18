// Package shell provides a disk backend that can also run commands on the
// host.
//
// Every command runs in a fresh "/bin/sh -c" from the workspace root (or
// ExecOptions.Dir), so `cd` and `export` do not carry over between commands;
// chain steps with `&&`. On Unix each command leads its own process group:
// when the shell exits, times out or is stopped, the whole group is killed
// (SIGTERM, then SIGKILL after DefaultKillGrace), so nothing a command spawns
// outlives it. Long-lived work belongs in background tasks (Starter), not in
// `cmd &`.
//
// Commands get no stdin, a small allowlisted environment plus
// NonInteractiveEnv (the host environment, which may hold API keys, is not
// inherited unless Options.InheritEnv is set), and output capped at a head and
// a tail so memory stays bounded.
//
// On platforms without POSIX process groups (Windows) execution is
// best-effort: only the shell process is killed, and the memory limit is
// enforced on Linux only.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/disk"
)

// Defaults for Options and for the kill sequence.
const (
	// DefaultTimeout bounds a command when neither ExecOptions nor Options
	// set a timeout.
	DefaultTimeout = 2 * time.Minute
	// DefaultMaxOutputBytes caps captured output; the first half and the last
	// half are kept.
	DefaultMaxOutputBytes = 256 << 10
	// DefaultKillGrace is how long a process group gets to exit after SIGTERM
	// before it is sent SIGKILL.
	DefaultKillGrace = 2 * time.Second
	// DefaultMemSample is how often the memory watchdog samples a group.
	DefaultMemSample = 500 * time.Millisecond
	// Shell is the interpreter every command runs under.
	Shell = "/bin/sh"
)

// NonInteractiveEnv keeps tools from paging, prompting or emitting terminal
// control codes, any of which would hang or garble a command with no
// terminal attached.
var NonInteractiveEnv = []string{"PAGER=cat", "GIT_PAGER=cat", "MANPAGER=cat", "GIT_TERMINAL_PROMPT=0",
	"DEBIAN_FRONTEND=noninteractive", "NO_COLOR=1", "TERM=dumb"}

// InheritedEnv names the host variables passed to commands by default: enough
// for tools to find binaries, a home and a locale, and nothing secret.
var InheritedEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TMPDIR", "TZ"}

// Options configures a Backend. The zero value is ready to use.
type Options struct {
	// Env is added to every command's environment after the inherited
	// variables and NonInteractiveEnv, so it wins over both;
	// ExecOptions.Env wins over it. nil adds nothing.
	Env map[string]string
	// InheritEnv passes the whole host environment instead of only
	// InheritedEnv. It may expose secrets such as API keys to commands.
	InheritEnv bool
	// Timeout applies to commands whose ExecOptions.Timeout is 0; 0 means
	// DefaultTimeout.
	Timeout time.Duration
	// MaxOutputBytes applies to commands whose ExecOptions.MaxOutputBytes is
	// 0; 0 means DefaultMaxOutputBytes.
	MaxOutputBytes int
	// MemoryLimitBytes kills a command whose process group's summed resident
	// memory exceeds it, sampled every DefaultMemSample. Linux only; 0
	// disables the watchdog.
	MemoryLimitBytes int64
	// DiskOptions configure the underlying disk backend; nil uses its
	// defaults.
	DiskOptions []disk.Option
}

// Backend is a disk backend that also runs commands; it implements
// backend.Backend, backend.MultiEditor, backend.Executor and backend.Starter.
type Backend struct {
	*disk.Backend
	opts Options
}

var (
	_ backend.Backend     = (*Backend)(nil)
	_ backend.MultiEditor = (*Backend)(nil)
	_ backend.Executor    = (*Backend)(nil)
	_ backend.Starter     = (*Backend)(nil)
)

// New returns a Backend rooted at root, which must be an existing directory.
func New(root string, opts Options) (*Backend, error) {
	d, err := disk.New(root, opts.DiskOptions...)
	if err != nil {
		return nil, fmt.Errorf("shell: %w", err)
	}
	return &Backend{Backend: d, opts: opts}, nil
}

// Execute runs cmd and waits for it. When ctx ends first the command's group
// is killed and ctx.Err() is returned with the output gathered so far.
func (b *Backend) Execute(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.ExecResult, error) {
	p, err := b.Start(ctx, cmd, opts)
	if err != nil {
		return backend.ExecResult{}, err
	}
	select {
	case <-p.Done():
	case <-ctx.Done():
		_ = p.Stop() // a failed signal still ends in Done via the kill escalation
		<-p.Done()
		res, _ := p.Result()
		return res, ctx.Err()
	}
	res, _ := p.Result()
	return res, nil
}

// Start runs cmd without waiting for it. ctx bounds only the start: the
// process ends on completion, timeout or Stop.
func (b *Backend) Start(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := b.workDir(opts.Dir)
	if err != nil {
		return nil, err
	}
	c := exec.Command(Shell, "-c", cmd) // nil Stdin reads from the null device
	c.Dir = dir
	c.Env = b.environ(opts.Env)
	return startProcess(c, b.maxOutput(opts), b.timeout(opts), b.opts.MemoryLimitBytes)
}

// workDir resolves a command's working directory, which must be an existing
// directory inside the root; empty means the root.
func (b *Backend) workDir(dir string) (string, error) {
	realPath, err := b.RealPath(dir)
	if err != nil {
		return "", fmt.Errorf("shell: dir: %w", err)
	}
	info, err := os.Stat(realPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("shell: dir %s: %w", dir, backend.ErrNotFound)
	case err != nil:
		return "", fmt.Errorf("shell: dir %s: %w", dir, err)
	case !info.IsDir():
		return "", fmt.Errorf("shell: dir %s: %w", dir, backend.ErrNotDir)
	}
	return realPath, nil
}

// environ builds a command's environment, lowest precedence first. exec.Cmd
// keeps only the last value of a repeated key, so later entries override.
func (b *Backend) environ(extra []string) []string {
	var env []string
	if b.opts.InheritEnv {
		env = os.Environ()
	} else {
		for _, name := range InheritedEnv {
			if value, ok := os.LookupEnv(name); ok {
				env = append(env, name+"="+value)
			}
		}
	}
	env = append(env, NonInteractiveEnv...)
	for _, key := range slices.Sorted(maps.Keys(b.opts.Env)) {
		env = append(env, key+"="+b.opts.Env[key])
	}
	return append(env, extra...)
}

func (b *Backend) timeout(opts backend.ExecOptions) time.Duration {
	return firstPositive(opts.Timeout, b.opts.Timeout, DefaultTimeout)
}

func (b *Backend) maxOutput(opts backend.ExecOptions) int {
	return firstPositive(opts.MaxOutputBytes, b.opts.MaxOutputBytes, DefaultMaxOutputBytes)
}

// firstPositive returns the first value above zero, or the last value.
func firstPositive[T int | time.Duration](values ...T) T {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return values[len(values)-1]
}
