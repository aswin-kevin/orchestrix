// Package sandbox provides a backend.Backend for a remote container or VM
// whose only primitive is running a shell command. An implementor supplies a
// backend.Executor; every file operation (read, write, edit, ls, glob, grep,
// delete) is built from POSIX sh scripts sent through it.
//
// Virtual paths map below Config.Root inside the sandbox: "/src/a.go" is
// <Root>/src/a.go. Paths are confined lexically (backend.Clean never climbs
// above "/"); symlinks inside the sandbox are followed by the commands, so
// the sandbox itself, not this package, is the security boundary.
//
// Quoting: every path reaches the shell as one POSIX single-quoted word built
// by a single helper, file content travels as base64 in chunks of ChunkBytes,
// and no caller data is ever placed in a printf format string. Hostile names
// (quotes, $(...), backticks, ";", newlines, leading "-", globs, non-UTF-8
// bytes) are therefore inert.
//
// Symlinks: commands follow them. Ls lists a symlink to a directory as a
// directory and a dangling symlink as a file of size 0 with a zero (Unix
// epoch) mtime, which Delete removes and Read reports as not found. Glob and
// Grep list regular files only, so they skip symlinks.
//
// Limits: modification times are read with whole-second resolution; a
// rewritten file gets default permissions rather than keeping its old mode;
// listings run wc and date per entry, so Glob and Grep over very large trees
// are slow. Grep prunes .git and node_modules directories, like backend/disk.
// Writes fill a temporary ".orx-tmp-<16 hex digits>" file next to the
// target, and listings hide names of exactly that form. Nothing sweeps them:
// a write cut off between chunks (the executor died, the sandbox restarted)
// leaves its temporary file behind, hidden but using space, until removed by
// hand. A file the sandbox user cannot read is listed with size 0 (and its
// real mtime); Grep skips it. A file that grows by more than a few KiB
// between the size check and the transfer in Read or Grep fails safely: the
// output exceeds the cap computed from the earlier size and the operation
// returns a truncation error, never partial content.
// A listing whose tools fail (an unsupported find flag, date -r erroring on
// an existing entry) is an error, never a silently empty result.
//
// Non-UTF-8 names: sizes and contents are read through shell redirection,
// which takes any bytes, but modification times need date -r FILE, which
// takes the name as an argument, and there is no portable way around that.
// Some userlands (the Rust coreutils that recent Ubuntu ships by default)
// refuse arguments that are not valid UTF-8. There, reading or listing such
// a name fails with "sandbox: this userland cannot handle non-UTF-8 file
// names: <path>" rather than with a raw tool error; GNU coreutils, busybox
// and the BSD tools handle any bytes.
//
// Background commands: the Backend New returns implements backend.Executor
// only. When the executor also implements backend.Starter, NewStarter
// returns a StarterBackend that starts commands the same way, which the jobs
// middleware (orchestrix.WithJobs) needs. orxdocker, a separate module,
// provides a Docker executor that does.
//
// Local implementation: wrapping a backend/shell Backend gives a sandbox on
// the host, useful for development and tests:
//
//	sh, err := shell.New(dir, shell.Config{})
//	...
//	b, err := sandbox.New(sh, sandbox.Config{Root: dir})
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

const (
	// DefaultRoot is the sandbox directory that backend path "/" maps to.
	DefaultRoot = "/workspace"
	// DefaultOpTimeout bounds each command a file operation sends.
	DefaultOpTimeout = 30 * time.Second
	// ChunkBytes is how many raw bytes one write command carries (64 KiB of
	// base64), which keeps every command far below argument-size limits.
	ChunkBytes = 48 << 10
)

// Config configures a Backend. The zero value uses DefaultRoot and
// DefaultOpTimeout.
type Config struct {
	// Root is the absolute, clean directory inside the sandbox that backend
	// path "/" maps to; "" means DefaultRoot. It need not exist yet: the
	// first Write creates it.
	Root string
	// OpTimeout bounds each command a file operation sends; 0 means
	// DefaultOpTimeout and a negative value is an error. It does not apply
	// to Execute.
	OpTimeout time.Duration
}

// Backend implements backend.Backend, backend.MultiEditor and
// backend.Executor using only exec.Execute. The sandbox needs a POSIX sh with
// mkdir, mv, rm, wc, tail (with -c), a true executable, base64 (with -d),
// find (with -H, -mindepth, -maxdepth and -exec ... {} +) and date -r FILE.
// GNU coreutils and findutils, busybox,
// macOS and FreeBSD provide them; OpenBSD and NetBSD, whose date -r takes
// only seconds, are not supported. With a userland that refuses non-UTF-8
// arguments, names that are not valid UTF-8 cannot be read or listed (see
// the package documentation).
type Backend struct {
	exec    backend.Executor
	root    string
	timeout time.Duration
}

var (
	_ backend.Backend     = (*Backend)(nil)
	_ backend.MultiEditor = (*Backend)(nil)
	_ backend.Executor    = (*Backend)(nil)
)

// New returns a Backend running its commands through exec. It fails when exec
// is nil, Root is not an absolute, clean path or OpTimeout is negative.
func New(exec backend.Executor, cfg Config) (*Backend, error) {
	if exec == nil {
		return nil, errors.New("sandbox: nil executor")
	}
	root := cfg.Root
	if root == "" {
		root = DefaultRoot
	}
	if clean, err := backend.Clean(root); err != nil || clean != root {
		return nil, fmt.Errorf("sandbox: root %q must be an absolute, clean path", cfg.Root)
	}
	if cfg.OpTimeout < 0 {
		return nil, errors.New("sandbox: OpTimeout is negative")
	}
	timeout := cfg.OpTimeout
	if timeout == 0 {
		timeout = DefaultOpTimeout
	}
	return &Backend{exec: exec, root: root, timeout: timeout}, nil
}

// Execute runs cmd in Root (or Root+opts.Dir) through exec. The directory is
// entered with a quoted cd rather than forwarded as opts.Dir, because Dir
// names a path in the executor's own namespace; if the cd fails, the command
// does not run. opts.Timeout, Env and MaxOutputBytes pass through unchanged.
func (b *Backend) Execute(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.ExecResult, error) {
	cmd, opts, err := b.prepare(cmd, opts)
	if err != nil {
		return backend.ExecResult{}, fmt.Errorf("sandbox: execute: %w", err)
	}
	return b.exec.Execute(ctx, cmd, opts)
}

// prepare prefixes cmd with a quoted cd into Root+opts.Dir and clears
// opts.Dir, which would name a path in the executor's own namespace.
func (b *Backend) prepare(cmd string, opts backend.ExecOptions) (string, backend.ExecOptions, error) {
	dir, err := cleanBase(opts.Dir)
	if err != nil {
		return "", opts, err
	}
	opts.Dir = ""
	// "|| exit" ending its own line, unlike "&&", also guards the later
	// lines of a multi-line command.
	return "cd " + quote(b.real(dir)) + " || exit\n" + cmd, opts, nil
}

// real maps a cleaned virtual path to its path inside the sandbox.
func (b *Backend) real(clean string) string {
	return path.Join(b.root, clean)
}

// virtual maps a sandbox path at or under the root back to a virtual path.
func (b *Backend) virtual(realPath string) (string, bool) {
	if b.root == "/" {
		return realPath, true
	}
	if realPath == b.root {
		return "/", true
	}
	if len(realPath) > len(b.root) && realPath[:len(b.root)] == b.root && realPath[len(b.root)] == '/' {
		return realPath[len(b.root):], true
	}
	return "", false
}

// cleanBase cleans a directory argument; empty means the root.
func cleanBase(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	return backend.Clean(p)
}

// run sends one file-operation script and maps its exit status: the script
// protocol codes become the backend sentinels wrapped with op and the virtual
// path, any other failure quotes the tail of the output. maxOutput caps the
// captured output; a truncated result is an error because it cannot be
// parsed.
func (b *Backend) run(ctx context.Context, op, p, script string, maxOutput int) (string, error) {
	res, err := b.exec.Execute(ctx, script, backend.ExecOptions{
		Timeout:        b.timeout,
		Env:            scriptEnv(),
		MaxOutputBytes: maxOutput,
	})
	if err != nil {
		return "", fmt.Errorf("sandbox: %s %s: %w", op, p, err)
	}
	if err := exitError(res); err != nil {
		if !res.TimedOut && res.ExitCode == exitFailed && rejectsNonUTF8(res.Output) {
			return "", nonUTF8Error(p)
		}
		return "", fmt.Errorf("sandbox: %s %s: %w", op, p, err)
	}
	if res.Truncated {
		return "", fmt.Errorf("sandbox: %s %s: output over %d bytes was truncated", op, p, maxOutput)
	}
	return res.Output, nil
}
