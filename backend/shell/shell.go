// Package shell provides a disk backend that can also run commands on the
// host.
//
// Every command runs in a fresh "/bin/sh -c" from the workspace root (or
// ExecOptions.Dir), so `cd` and `export` do not carry over between commands;
// chain steps with `&&`. On Unix each command leads its own process group:
// when the shell exits, times out or is stopped, the whole group is killed
// (SIGTERM, then SIGKILL after DefaultKillGrace), so nothing a command spawns
// inside the group outlives it. Long-lived work belongs in background tasks
// (Starter), not in `cmd &`.
//
// This is cleanup, not containment: a process that leaves the group (setsid,
// a daemonizing double fork, a new session) is neither tracked nor killed and
// keeps running after the command ends. Execute waits at most
// DefaultKillGrace for such a process to release the output pipe, then
// returns without it. Use an OS-level sandbox when commands must not escape.
//
// Commands get no stdin, a small allowlisted environment (InheritedEnv) plus
// the variables of NonInteractiveEnv and SafeGitEnv (the host environment,
// which may hold API keys, is not inherited unless Config.InheritEnv is
// set), and output capped at a head and a tail so memory stays bounded.
// Those three functions return copies: changing what they return does not
// change what commands get.
//
// Commands need Shell (/bin/sh), so they run on Unix only. On Windows the
// file operations work, but every command fails to start; use a disk
// backend there, or run commands in a container (orxdocker). The memory
// limit is enforced on Linux only.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/disk"
	"github.com/aswin-kevin/orchestrix/internal/hostenv"
)

// Defaults for Config and for the kill sequence.
const (
	// DefaultTimeout bounds a command when neither ExecOptions nor Config
	// set a timeout.
	DefaultTimeout = 2 * time.Minute
	// DefaultMaxOutputBytes caps captured output; the first half and the last
	// half are kept.
	DefaultMaxOutputBytes = 256 << 10
	// DefaultKillGrace is how long a process group gets to exit after SIGTERM
	// before it is sent SIGKILL; see backend.DefaultKillGrace.
	DefaultKillGrace = backend.DefaultKillGrace
	// DefaultMemSample is how often the memory watchdog samples a group.
	DefaultMemSample = 500 * time.Millisecond
	// Shell is the interpreter every command runs under.
	Shell = "/bin/sh"
)

// nonInteractiveEnv backs NonInteractiveEnv; commands read it, so callers
// changing a returned map cannot change what commands get.
var nonInteractiveEnv = map[string]string{
	"PAGER": "cat", "GIT_PAGER": "cat", "MANPAGER": "cat", "GIT_TERMINAL_PROMPT": "0",
	"DEBIAN_FRONTEND": "noninteractive", "NO_COLOR": "1", "TERM": "dumb",
}

// safeGitEnv backs SafeGitEnv; commands read it, so callers changing a
// returned map cannot weaken the git hardening.
var safeGitEnv = map[string]string{
	"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.bareRepository", "GIT_CONFIG_VALUE_0": "explicit",
}

// NonInteractiveEnv returns the variables that keep tools from paging,
// prompting or emitting terminal control codes, any of which would hang or
// garble a command with no terminal attached. Each call returns a fresh copy.
func NonInteractiveEnv() map[string]string { return maps.Clone(nonInteractiveEnv) }

// SafeGitEnv returns the variables that set safe.bareRepository=explicit, so
// git uses a bare repository only when one is named with --git-dir or
// GIT_DIR, never one it merely finds: a directory with HEAD, config, objects
// and refs, which an agent could write with file tools, would otherwise let
// its config run commands (an alias, core.fsmonitor) from any git call made
// inside it.
// Configuration set through the environment overrides the repository's, and
// git 2.38 and later honour this setting. It replaces any GIT_CONFIG_COUNT
// settings an inherited environment carries; Config.Env or ExecOptions.Env
// can restore them. Each call returns a fresh copy.
func SafeGitEnv() map[string]string { return maps.Clone(safeGitEnv) }

// InheritedEnv returns the names of the host variables passed to commands by
// default: enough for tools to find binaries, a home and a locale, and
// nothing secret. MCP stdio servers get the same list. Each call returns a
// fresh copy.
func InheritedEnv() []string { return hostenv.DefaultNames() }

// Config holds the optional settings of a Backend. The zero value is valid.
type Config struct {
	// Env is added to every command's environment after the inherited
	// variables, NonInteractiveEnv and SafeGitEnv, so it wins over them;
	// ExecOptions.Env wins over it. nil adds nothing. A key that is empty
	// or holds '=' or NUL makes New fail, and in ExecOptions.Env makes
	// Execute and Start fail.
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
	// Disk configures the underlying disk backend; the zero value uses its
	// defaults.
	Disk disk.Config
}

// Backend is a disk backend that also runs commands; it implements
// backend.Backend, backend.MultiEditor, backend.Executor and backend.Starter.
type Backend struct {
	// Backend is the disk backend that provides the file operations.
	*disk.Backend
	cfg Config
}

var (
	_ backend.Backend     = (*Backend)(nil)
	_ backend.MultiEditor = (*Backend)(nil)
	_ backend.Executor    = (*Backend)(nil)
	_ backend.Starter     = (*Backend)(nil)
)

// New returns a Backend rooted at root, which must be an existing directory.
func New(root string, cfg Config) (*Backend, error) {
	switch {
	case cfg.Timeout < 0:
		return nil, errors.New("shell: Timeout is negative")
	case cfg.MaxOutputBytes < 0:
		return nil, errors.New("shell: MaxOutputBytes is negative")
	case cfg.MemoryLimitBytes < 0:
		return nil, errors.New("shell: MemoryLimitBytes is negative")
	}
	if err := checkEnv(cfg.Env); err != nil {
		return nil, err
	}
	d, err := disk.New(root, cfg.Disk)
	if err != nil {
		return nil, fmt.Errorf("shell: %w", err)
	}
	return &Backend{Backend: d, cfg: cfg}, nil
}

// Execute runs cmd and waits for it. When ctx ends first the command's group
// is killed and ctx.Err() is returned with the output gathered so far.
// Killing takes time: after ctx ends, Execute may block for up to about three
// times DefaultKillGrace (SIGTERM grace, SIGKILL of the leftovers, and the
// wait for the output pipe) before it returns.
func (b *Backend) Execute(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.ExecResult, error) {
	p, err := b.Start(ctx, cmd, opts)
	if err != nil {
		return backend.ExecResult{}, err
	}
	select {
	case <-p.Done():
	case <-ctx.Done():
		_ = p.Stop(ctx) // a failed signal still ends in Done via the kill escalation
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
	if err := checkEnv(opts.Env); err != nil {
		return nil, err
	}
	dir, err := b.workDir(opts.Dir)
	if err != nil {
		return nil, err
	}
	c := exec.Command(Shell, "-c", cmd) // nil Stdin reads from the null device
	c.Dir = dir
	c.Env = b.environ(opts.Env)
	return startProcess(c, b.maxOutput(opts), b.timeout(opts), b.cfg.MemoryLimitBytes)
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

// checkEnv rejects an environment key that is empty or holds '=' or NUL.
func checkEnv(env map[string]string) error {
	if key, bad := hostenv.InvalidKey(env); bad {
		return fmt.Errorf("shell: env key %q is invalid", key)
	}
	return nil
}

// environ builds a command's environment, lowest precedence first. exec.Cmd
// keeps only the last value of a repeated key, so later entries override.
func (b *Backend) environ(extra map[string]string) []string {
	env := hostenv.Inherited(hostenv.DefaultNames(), b.cfg.InheritEnv)
	env = append(env, hostenv.Pairs(nonInteractiveEnv)...)
	env = append(env, hostenv.Pairs(safeGitEnv)...)
	env = append(env, hostenv.Pairs(b.cfg.Env)...)
	return append(env, hostenv.Pairs(extra)...)
}

func (b *Backend) timeout(opts backend.ExecOptions) time.Duration {
	return firstPositive(opts.Timeout, b.cfg.Timeout, DefaultTimeout)
}

func (b *Backend) maxOutput(opts backend.ExecOptions) int {
	return firstPositive(opts.MaxOutputBytes, b.cfg.MaxOutputBytes, DefaultMaxOutputBytes)
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
