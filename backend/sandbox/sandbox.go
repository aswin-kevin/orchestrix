// Package sandbox provides a backend.Backend for a remote container or VM
// whose only primitive is running a shell command. An implementor supplies a
// backend.Executor; every file operation (read, write, edit, ls, glob, grep,
// delete) is built from POSIX sh scripts sent through it.
//
// Virtual paths map below Options.Root inside the sandbox: "/src/a.go" is
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
// Writes fill a temporary ".orx-tmp-<hex>" file next to the target, and
// listings hide such names. A file that grows by more than a few KiB between
// the size check and the transfer in Read or Grep fails safely: the output
// exceeds the cap computed from the earlier size and the operation returns a
// truncation error, never partial content.
// A listing whose tools fail (an unsupported find flag, date -r erroring on
// an existing entry) is an error, never a silently empty result.
//
// Local implementation: wrapping a backend/shell Backend gives a sandbox on
// the host, useful for development and tests:
//
//	sh, err := shell.New(dir, shell.Options{})
//	...
//	b, err := sandbox.New(sh, sandbox.Options{Root: dir})
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

// Options configures a Backend. The zero value uses DefaultRoot and
// DefaultOpTimeout.
type Options struct {
	// Root is the absolute, clean directory inside the sandbox that backend
	// path "/" maps to; "" means DefaultRoot. It need not exist yet: the
	// first Write creates it.
	Root string
	// OpTimeout bounds each command a file operation sends; 0 means
	// DefaultOpTimeout. It does not apply to Execute.
	OpTimeout time.Duration
}

// Backend implements backend.Backend, backend.MultiEditor and
// backend.Executor using only exec.Execute. The sandbox needs a POSIX sh with
// mkdir, mv, rm, wc, base64 (with -d), find (with -H, -mindepth, -maxdepth
// and -exec ... {} +) and date -r FILE. GNU coreutils and findutils, busybox,
// macOS and FreeBSD provide them; OpenBSD and NetBSD, whose date -r takes
// only seconds, are not supported.
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
// is nil or Root is not an absolute, clean path.
func New(exec backend.Executor, opts Options) (*Backend, error) {
	if exec == nil {
		return nil, errors.New("sandbox: nil executor")
	}
	root := opts.Root
	if root == "" {
		root = DefaultRoot
	}
	if clean, err := backend.Clean(root); err != nil || clean != root {
		return nil, fmt.Errorf("sandbox: root %q must be an absolute, clean path", opts.Root)
	}
	timeout := opts.OpTimeout
	if timeout <= 0 {
		timeout = DefaultOpTimeout
	}
	return &Backend{exec: exec, root: root, timeout: timeout}, nil
}

// Execute runs cmd in Root (or Root+opts.Dir) through exec. The directory is
// entered with a quoted cd rather than forwarded as opts.Dir, because Dir
// names a path in the executor's own namespace; if the cd fails, the command
// does not run. opts.Timeout, Env and MaxOutputBytes pass through unchanged.
func (b *Backend) Execute(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.ExecResult, error) {
	dir, err := cleanBase(opts.Dir)
	if err != nil {
		return backend.ExecResult{}, fmt.Errorf("sandbox: execute: %w", err)
	}
	opts.Dir = ""
	// "|| exit" ending its own line, unlike "&&", also guards the later
	// lines of a multi-line command.
	return b.exec.Execute(ctx, "cd "+quote(b.real(dir))+" || exit\n"+cmd, opts)
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
		Env:            scriptEnv,
		MaxOutputBytes: maxOutput,
	})
	if err != nil {
		return "", fmt.Errorf("sandbox: %s %s: %w", op, p, err)
	}
	if err := exitError(res); err != nil {
		return "", fmt.Errorf("sandbox: %s %s: %w", op, p, err)
	}
	if res.Truncated {
		return "", fmt.Errorf("sandbox: %s %s: output over %d bytes was truncated", op, p, maxOutput)
	}
	return res.Output, nil
}
