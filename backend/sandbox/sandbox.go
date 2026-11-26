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
