package sandbox_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/sandbox"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/internal/hostenv"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
)

var (
	_ backend.Backend     = (*sandbox.Backend)(nil)
	_ backend.MultiEditor = (*sandbox.Backend)(nil)
	_ backend.Executor    = (*sandbox.Backend)(nil)
)

// skipWithoutSh skips tests that spawn /bin/sh on Windows.
func skipWithoutSh(t *testing.T) {
	t.Helper()
	testutil.RequireUnixShell(t)
}

// localExec runs commands with /bin/sh in a private temp directory, so a
// quoting bug can never write into the repository, and records every command.
type localExec struct {
	dir  string
	env  []string // extra environment, e.g. a PATH with fake tools
	mu   sync.Mutex
	cmds []string
}

func newLocalExec(t *testing.T) *localExec {
	t.Helper()
	skipWithoutSh(t)
	return &localExec{dir: t.TempDir()}
}

func (e *localExec) Execute(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.ExecResult, error) {
	e.mu.Lock()
	e.cmds = append(e.cmds, cmd)
	e.mu.Unlock()
	if opts.Dir != "" {
		return backend.ExecResult{}, errors.New("localExec: sandbox forwarded Dir")
	}
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Dir = e.dir
	c.Env = append(append(os.Environ(), e.env...), hostenv.Pairs(opts.Env)...)
	out, err := c.CombinedOutput()
	res := backend.ExecResult{Output: string(out)}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case err != nil:
		return res, err
	}
	return res, nil
}

// commands returns the recorded commands and forgets them.
func (e *localExec) commands() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	cmds := e.cmds
	e.cmds = nil
	return cmds
}

func newSandbox(t *testing.T, exec backend.Executor, root string) *sandbox.Backend {
	t.Helper()
	b, err := sandbox.New(exec, sandbox.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readBytes(t *testing.T, b backend.Backend, p string) []byte {
	t.Helper()
	ctx := t.Context()
	res, err := b.Read(ctx, p, 0, 0)
	if err != nil {
		t.Fatalf("Read(%q): %v", p, err)
	}
	if res.Binary {
		return res.Data
	}
	return []byte(strings.Join(res.Lines, "\n"))
}

func paths(infos []backend.FileInfo) []string {
	out := make([]string, len(infos))
	for i, fi := range infos {
		out[i] = fi.Path
	}
	return out
}

func TestSandboxConformance(t *testing.T) {
	skipWithoutSh(t)
	backendtest.RunWith(t, func(t *testing.T) backend.Backend {
		return newSandbox(t, newLocalExec(t), t.TempDir())
	}, backendtest.Options{ModTimeResolution: time.Second})
}

func TestSandboxOverShell(t *testing.T) {
	ctx := t.Context()
	skipWithoutSh(t)
	dir := t.TempDir()
	sh, err := shell.New(dir, shell.Config{})
	if err != nil {
		t.Fatal(err)
	}
	b := newSandbox(t, sh, dir)
	if err := b.Write(ctx, "/src/main.go", []byte("package main\n")); err != nil {
		t.Fatal(err)
	}
	if got := string(readBytes(t, b, "/src/main.go")); got != "package main" {
		t.Fatalf("Read = %q", got)
	}
	host, err := os.ReadFile(filepath.Join(dir, "src", "main.go"))
	if err != nil || string(host) != "package main\n" {
		t.Fatalf("host file = %q, %v", host, err)
	}
	infos, err := b.Ls(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/src"}; !slices.Equal(paths(infos), want) || !infos[0].IsDir {
		t.Fatalf("Ls(/) = %+v, want dir %q", infos, want)
	}
	res, err := b.Execute(ctx, "cat main.go", backend.ExecOptions{Dir: "/src"})
	if err != nil || res.Output != "package main\n" {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
}

func TestSandboxExecuteDir(t *testing.T) {
	ctx := t.Context()
	e := newLocalExec(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := newSandbox(t, e, root)
	res, err := b.Execute(ctx, "pwd", backend.ExecOptions{Dir: "/sub"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(res.Output), root+"/sub"; got != want {
		t.Fatalf("pwd = %q, want %q", got, want)
	}
	res, err = b.Execute(ctx, "pwd", backend.ExecOptions{})
	if err != nil || strings.TrimSpace(res.Output) != root {
		t.Fatalf("pwd at root = %+v, %v", res, err)
	}
	// A failed cd stops a multi-line command before it runs anywhere else.
	res, err = b.Execute(ctx, "true\ntouch escaped", backend.ExecOptions{Dir: "/missing"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("cd into a missing dir succeeded: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "escaped")); err == nil {
		t.Fatal("command ran outside the sandbox root")
	}
}

func TestNewValidation(t *testing.T) {
	skipWithoutSh(t)
	e := newLocalExec(t)
	if _, err := sandbox.New(nil, sandbox.Config{}); err == nil {
		t.Fatal("nil executor accepted")
	}
	for _, root := range []string{"rel", "/a/../b", "/a/", "/a\x00b"} {
		if _, err := sandbox.New(e, sandbox.Config{Root: root}); err == nil {
			t.Fatalf("Root %q accepted", root)
		}
	}
	if _, err := sandbox.New(e, sandbox.Config{}); err != nil {
		t.Fatalf("default root: %v", err)
	}
}

// fakeExec answers every command with a fixed result.
type fakeExec struct {
	res backend.ExecResult
	err error
}

func (f fakeExec) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	return f.res, f.err
}

// recordingExec records the options of every command and answers each with
// exit code 3 (not found).
type recordingExec struct {
	opts []backend.ExecOptions
}

func (r *recordingExec) Execute(_ context.Context, _ string, opts backend.ExecOptions) (backend.ExecResult, error) {
	r.opts = append(r.opts, opts)
	return backend.ExecResult{ExitCode: 3}, nil
}

func TestScriptEnvFreshPerCall(t *testing.T) {
	ctx := t.Context()
	rec := &recordingExec{}
	b := newSandbox(t, rec, "/work")
	if _, err := b.Read(ctx, "/f.txt", 0, 0); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Read = %v, want ErrNotFound", err)
	}
	if len(rec.opts) != 1 {
		t.Fatalf("Read ran %d commands, want 1", len(rec.opts))
	}
	rec.opts[0].Env["LC_ALL"] = "en_US.UTF-8"
	rec.opts[0].Env["EXTRA"] = "1"
	if _, err := b.Read(ctx, "/f.txt", 0, 0); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Read = %v, want ErrNotFound", err)
	}
	if got, want := rec.opts[1].Env, map[string]string{"LC_ALL": "C"}; !maps.Equal(got, want) {
		t.Fatalf("second Read's env = %v, want %v", got, want)
	}
}

func TestSandboxErrorMapping(t *testing.T) {
	ctx := t.Context()
	boom := errors.New("transport down")
	tests := []struct {
		name string
		exec fakeExec
		want error
		text string
	}{
		{"exit 3", fakeExec{res: backend.ExecResult{ExitCode: 3}}, backend.ErrNotFound, ""},
		{"exit 4", fakeExec{res: backend.ExecResult{ExitCode: 4}}, backend.ErrIsDir, ""},
		{"exit 5", fakeExec{res: backend.ExecResult{ExitCode: 5}}, backend.ErrNotDir, ""},
		{"exit 1", fakeExec{res: backend.ExecResult{ExitCode: 1, Output: "mv: disk full"}}, nil, "mv: disk full"},
		{"executor error", fakeExec{err: boom}, boom, ""},
		{"timeout", fakeExec{res: backend.ExecResult{ExitCode: -1, TimedOut: true}}, nil, "timed out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newSandbox(t, tt.exec, "/work")
			ops := map[string]error{}
			_, ops["read"] = b.Read(ctx, "/a/f.txt", 0, 0)
			ops["write"] = b.Write(ctx, "/a/f.txt", []byte("x"))
			_, ops["edit"] = b.Edit(ctx, "/a/f.txt", "x", "y", false)
			ops["delete"] = b.Delete(ctx, "/a/f.txt")
			_, ops["ls"] = b.Ls(ctx, "/a")
			_, ops["glob"] = b.Glob(ctx, "*", "/a")
			_, ops["grep"] = b.Grep(ctx, "x", backend.GrepOptions{Path: "/a"})
			for op, err := range ops {
				switch {
				case err == nil:
					t.Errorf("%s: no error", op)
				case tt.want != nil && !errors.Is(err, tt.want):
					t.Errorf("%s: error = %v, want %v", op, err, tt.want)
				case !strings.Contains(err.Error(), tt.text):
					t.Errorf("%s: error = %v, want it to contain %q", op, err, tt.text)
				case tt.want != nil && tt.want != boom && !strings.Contains(err.Error(), "/a"):
					t.Errorf("%s: error = %v, want it to name the path", op, err)
				}
			}
		})
	}
}

func TestSandboxTruncatedOutputIsError(t *testing.T) {
	ctx := t.Context()
	b := newSandbox(t, fakeExec{res: backend.ExecResult{Output: "5\n1700000000\n", Truncated: true}}, "/work")
	if _, err := b.Read(ctx, "/f.txt", 0, 0); err == nil {
		t.Fatal("truncated output accepted")
	}
}

func TestSandboxDeleteRootRefused(t *testing.T) {
	ctx := t.Context()
	b := newSandbox(t, newLocalExec(t), t.TempDir())
	for _, p := range []string{"/", "/..", "."} {
		if err := b.Delete(ctx, p); !errors.Is(err, backend.ErrInvalidPath) {
			t.Fatalf("Delete(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestSandboxWriteRootIsDir(t *testing.T) {
	ctx := t.Context()
	b := newSandbox(t, fakeExec{}, "/work")
	if err := b.Write(ctx, "/", []byte("x")); !errors.Is(err, backend.ErrIsDir) {
		t.Fatalf("Write(/) = %v, want ErrIsDir", err)
	}
}

func TestSandboxWriteLongName(t *testing.T) {
	ctx := t.Context()
	b := newSandbox(t, newLocalExec(t), t.TempDir())
	name := "/" + strings.Repeat("n", 240)
	if err := b.Write(ctx, name, []byte("long")); err != nil {
		t.Fatalf("Write(240-byte name): %v", err)
	}
	if got := string(readBytes(t, b, name)); got != "long" {
		t.Fatalf("Read = %q", got)
	}
}

func TestNewRejectsNegativeOpTimeout(t *testing.T) {
	_, err := sandbox.New(fakeExec{}, sandbox.Config{OpTimeout: -time.Second})
	if err == nil || err.Error() != "sandbox: OpTimeout is negative" {
		t.Fatalf("New = %v, want \"sandbox: OpTimeout is negative\"", err)
	}
	if _, err := sandbox.NewStarter(&fakeStarter{}, sandbox.Config{OpTimeout: -1}); err == nil {
		t.Fatal("NewStarter accepted a negative OpTimeout")
	}
}

func TestSandboxExecutorConformance(t *testing.T) {
	skipWithoutSh(t)
	backendtest.RunExecutor(t, func(t *testing.T) backend.Executor {
		dir := t.TempDir()
		sh, err := shell.New(dir, shell.Config{})
		if err != nil {
			t.Fatal(err)
		}
		return newSandbox(t, sh, dir)
	})
}

func TestSandboxStarterConformance(t *testing.T) {
	skipWithoutSh(t)
	backendtest.RunStarter(t, func(t *testing.T) backend.Starter {
		dir := t.TempDir()
		sh, err := shell.New(dir, shell.Config{})
		if err != nil {
			t.Fatal(err)
		}
		b, err := sandbox.NewStarter(sh, sandbox.Config{Root: dir})
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}
