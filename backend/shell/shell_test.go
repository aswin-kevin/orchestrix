//go:build unix

package shell_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/disk"
	"github.com/aswin-kevin/orchestrix/backend/shell"
)

// goneWithin bounds how long a test waits for a killed process to disappear.
const goneWithin = 2 * time.Second

var (
	_ backend.Backend     = (*shell.Backend)(nil)
	_ backend.Executor    = (*shell.Backend)(nil)
	_ backend.Starter     = (*shell.Backend)(nil)
	_ backend.MultiEditor = (*shell.Backend)(nil)
)

// newShell returns a backend over a fresh temp dir and the dir's symlink-free
// path, which is what commands see as their working directory.
func newShell(t *testing.T, cfg shell.Config) (*shell.Backend, string) {
	t.Helper()
	b, err := shell.New(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.RealPath("")
	if err != nil {
		t.Fatal(err)
	}
	return b, root
}

func execute(t *testing.T, b *shell.Backend, cmd string, opts backend.ExecOptions) backend.ExecResult {
	t.Helper()
	res, err := b.Execute(context.Background(), cmd, opts)
	if err != nil {
		t.Fatalf("Execute(%q): %v", cmd, err)
	}
	return res
}

// firstLinePID parses the pid a test command printed with `echo $$` or
// `echo $!` as its first output line.
func firstLinePID(t *testing.T, output string) int {
	t.Helper()
	line, _, _ := strings.Cut(output, "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("no pid on the first line of %q", output)
	}
	return pid
}

// requireGone fails unless kill(target, 0) reports ESRCH within goneWithin,
// killing the target if it is still there. A negative target names a process
// group.
func requireGone(t *testing.T, target int) {
	t.Helper()
	deadline := time.Now().Add(goneWithin)
	for {
		if err := syscall.Kill(target, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			// never leave the leak running
			_ = syscall.Kill(target, syscall.SIGKILL)
			t.Fatalf("process (group) %d still exists", target)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestShellConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		b, _ := newShell(t, shell.Config{})
		return b
	})
}

func TestExecuteExitCodeAndOutput(t *testing.T) {
	b, _ := newShell(t, shell.Config{})
	res := execute(t, b, "echo hi; echo err >&2; exit 3", backend.ExecOptions{})
	if res.ExitCode != 3 || res.TimedOut || res.Truncated {
		t.Errorf("result = %+v, want exit 3, not timed out or truncated", res)
	}
	if !strings.Contains(res.Output, "hi\n") || !strings.Contains(res.Output, "err\n") {
		t.Errorf("output %q must hold stdout and stderr", res.Output)
	}
}

func TestExecuteStdinIsEmpty(t *testing.T) {
	b, _ := newShell(t, shell.Config{})
	res := execute(t, b, "cat; echo done", backend.ExecOptions{Timeout: 5 * time.Second})
	if res.Output != "done\n" || res.TimedOut {
		t.Errorf("result = %+v; a stdin reader must see EOF at once", res)
	}
}

func TestExecuteRunsInRootAndDir(t *testing.T) {
	b, root := newShell(t, shell.Config{})
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := execute(t, b, "pwd", backend.ExecOptions{}); res.Output != root+"\n" {
		t.Errorf("pwd = %q, want root %q", res.Output, root)
	}
	if res := execute(t, b, "pwd", backend.ExecOptions{Dir: "/sub"}); res.Output != filepath.Join(root, "sub")+"\n" {
		t.Errorf("pwd in /sub = %q", res.Output)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for dir, want := range map[string]error{
		// cleaned to <root>/x, which does not exist
		"/../x":     backend.ErrNotFound,
		"/missing":  backend.ErrNotFound,
		"/file.txt": backend.ErrNotDir,
		"/link":     backend.ErrOutsideRoot,
	} {
		if _, err := b.Execute(ctx, "pwd", backend.ExecOptions{Dir: dir}); !errors.Is(err, want) {
			t.Errorf("Execute with Dir %q = %v, want %v", dir, err, want)
		}
		if _, err := b.Start(ctx, "pwd", backend.ExecOptions{Dir: dir}); !errors.Is(err, want) {
			t.Errorf("Start with Dir %q = %v, want %v", dir, err, want)
		}
	}
}

func TestEnvAllowlist(t *testing.T) {
	t.Setenv("ORX_SECRET_TEST", "leaked")
	b, _ := newShell(t, shell.Config{})
	res := execute(t, b, "env", backend.ExecOptions{})
	if strings.Contains(res.Output, "ORX_SECRET_TEST") {
		t.Errorf("host secret leaked into the command env:\n%s", res.Output)
	}
	if !containsLine(strings.Split(res.Output, "\n"), "PATH="+os.Getenv("PATH")) {
		t.Errorf("env lacks the inherited PATH:\n%s", res.Output)
	}

	inherit, _ := newShell(t, shell.Config{InheritEnv: true})
	if res := execute(t, inherit, "env", backend.ExecOptions{}); !containsLine(strings.Split(res.Output, "\n"), "ORX_SECRET_TEST=leaked") {
		t.Errorf("InheritEnv must pass the host env:\n%s", res.Output)
	}
}

// TestEnvPrecedence pins the order: inherited host variables, then
// NonInteractiveEnv, then SafeGitEnv, then Config.Env, then
// ExecOptions.Env, each overriding the ones before.
func TestEnvPrecedence(t *testing.T) {
	b, _ := newShell(t, shell.Config{Env: map[string]string{"A": "1", "B": "options", "TERM": "xterm"}})
	res := execute(t, b, "env", backend.ExecOptions{Env: map[string]string{"B": "exec"}})
	lines := strings.Split(res.Output, "\n")
	for _, want := range []string{
		"A=1", "B=exec", "TERM=xterm", "PAGER=cat", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit",
	} {
		if !containsLine(lines, want) {
			t.Errorf("env lacks %q:\n%s", want, res.Output)
		}
	}
	for _, unwanted := range []string{"B=options", "TERM=dumb"} {
		if containsLine(lines, unwanted) {
			t.Errorf("env holds overridden %q:\n%s", unwanted, res.Output)
		}
	}

	// An inherited environment's git settings are replaced by SafeGitEnv, and
	// an inherited TERM by NonInteractiveEnv; Config.Env can restore them.
	t.Setenv("GIT_CONFIG_COUNT", "5")
	t.Setenv("GIT_CONFIG_VALUE_0", "all")
	t.Setenv("TERM", "xterm-256color")
	inherit, _ := newShell(t, shell.Config{InheritEnv: true})
	lines = strings.Split(execute(t, inherit, "env", backend.ExecOptions{}).Output, "\n")
	for _, want := range []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_VALUE_0=explicit", "TERM=dumb"} {
		if !containsLine(lines, want) {
			t.Errorf("inherited env: lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	restore, _ := newShell(t, shell.Config{InheritEnv: true, Env: map[string]string{"GIT_CONFIG_COUNT": "0"}})
	lines = strings.Split(execute(t, restore, "env", backend.ExecOptions{}).Output, "\n")
	if !containsLine(lines, "GIT_CONFIG_COUNT=0") {
		t.Errorf("Config.Env did not override SafeGitEnv:\n%s", strings.Join(lines, "\n"))
	}
	plain, _ := newShell(t, shell.Config{})
	res = execute(t, plain, "env", backend.ExecOptions{Env: map[string]string{"GIT_CONFIG_COUNT": "0"}})
	if !containsLine(strings.Split(res.Output, "\n"), "GIT_CONFIG_COUNT=0") {
		t.Errorf("ExecOptions.Env did not override SafeGitEnv:\n%s", res.Output)
	}
}

// TestGitHardeningSurvivesCallerMutation checks that changing the map
// SafeGitEnv returns does not weaken the environment commands get.
func TestGitHardeningSurvivesCallerMutation(t *testing.T) {
	env := shell.SafeGitEnv()
	env["GIT_CONFIG_VALUE_0"] = "all"
	requireBareRepositoryIgnored(t)
}

// TestGitIgnoresImplicitBareRepository plants a bare repository whose config
// defines an alias that runs a command, and checks that git, run inside it,
// does not use it.
func TestGitIgnoresImplicitBareRepository(t *testing.T) {
	requireBareRepositoryIgnored(t)
}

// requireBareRepositoryIgnored runs TestGitIgnoresImplicitBareRepository's
// scenario.
func requireBareRepositoryIgnored(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	b, root := newShell(t, shell.Config{})
	marker := filepath.Join(root, "ran")
	files := map[string]string{
		"evil/HEAD":   "ref: refs/heads/main\n",
		"evil/config": "[core]\n\tbare = true\n[alias]\n\tst = \"!touch " + marker + "\"\n",
	}
	for _, dir := range []string{"evil/objects", "evil/refs/heads"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, b, "git -C evil st", backend.ExecOptions{})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git used a bare repository it only discovered, and ran its alias")
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func TestTimeoutKillsGroup(t *testing.T) {
	cases := map[string]struct {
		cfg  shell.Config
		exec backend.ExecOptions
	}{
		"exec timeout":    {exec: backend.ExecOptions{Timeout: 200 * time.Millisecond}},
		"default timeout": {cfg: shell.Config{Timeout: 200 * time.Millisecond}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, _ := newShell(t, tc.cfg)
			start := time.Now()
			res := execute(t, b, "echo $$; sleep 30", tc.exec)
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("timed-out command took %s", elapsed)
			}
			if !res.TimedOut || res.ExitCode != -1 {
				t.Errorf("result = %+v, want TimedOut and exit -1", res)
			}
			requireGone(t, -firstLinePID(t, res.Output))
		})
	}
}

func TestTimeoutEscalatesToKill(t *testing.T) {
	b, _ := newShell(t, shell.Config{})
	start := time.Now()
	// The ignored SIGTERM is inherited by sleep, so only SIGKILL ends the
	// group.
	res := execute(t, b, "trap '' TERM; echo $$; sleep 30", backend.ExecOptions{Timeout: 200 * time.Millisecond})
	if elapsed := time.Since(start); elapsed > shell.DefaultKillGrace+3*time.Second {
		t.Errorf("SIGTERM-immune command took %s", elapsed)
	}
	if !res.TimedOut {
		t.Errorf("result = %+v, want TimedOut", res)
	}
	requireGone(t, -firstLinePID(t, res.Output))
}

func TestGrandchildKilled(t *testing.T) {
	b, _ := newShell(t, shell.Config{})
	start := time.Now()
	res := execute(t, b, "sleep 1000 & echo $!", backend.ExecOptions{})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Execute waited %s for a background grandchild", elapsed)
	}
	requireGone(t, firstLinePID(t, res.Output))

	// A grandchild that detached from the output pipe is still reaped.
	res = execute(t, b, "sleep 1000 >/dev/null 2>&1 & echo $!", backend.ExecOptions{})
	requireGone(t, firstLinePID(t, res.Output))
}

func TestOutputCapHeadAndTail(t *testing.T) {
	b, _ := newShell(t, shell.Config{})
	res := execute(t, b, "seq 1 200000", backend.ExecOptions{MaxOutputBytes: 4096})
	if !strings.HasPrefix(res.Output, "1\n2\n") || !strings.HasSuffix(res.Output, "\n200000\n") {
		t.Errorf("output must keep head and tail, got %q…%q", res.Output[:20], res.Output[len(res.Output)-20:])
	}
	if !res.Truncated || !strings.Contains(res.Output, " bytes omitted]") {
		t.Errorf("Truncated = %v, output must name the omitted bytes", res.Truncated)
	}
	if len(res.Output) > 4096+64 {
		t.Errorf("capped output is %d bytes", len(res.Output))
	}

	capped, _ := newShell(t, shell.Config{MaxOutputBytes: 100})
	if res := execute(t, capped, "seq 1 1000", backend.ExecOptions{}); !res.Truncated || len(res.Output) > 100+64 {
		t.Errorf("Config.MaxOutputBytes ignored: %d bytes, truncated %v", len(res.Output), res.Truncated)
	}
}

func TestStartOutputWhileRunningAndStop(t *testing.T) {
	b, _ := newShell(t, shell.Config{})
	p, err := b.Start(context.Background(), "echo $$; echo started; sleep 30", backend.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(goneWithin)
	for !strings.Contains(p.Output(), "started") {
		if time.Now().After(deadline) {
			t.Fatalf("output while running = %q", p.Output())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := p.Result(); ok {
		t.Fatal("Result must not be final while the command runs")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(goneWithin):
		t.Fatal("Done not closed after Stop")
	}
	res, ok := p.Result()
	if !ok || !strings.Contains(res.Output, "started") || res.TimedOut {
		t.Errorf("Result = %+v, %v", res, ok)
	}
	requireGone(t, -firstLinePID(t, res.Output))
	if err := p.Stop(context.Background()); err != nil {
		t.Errorf("Stop after Done = %v, want nil", err)
	}
}

func TestStartOutlivesContext(t *testing.T) {
	b, _ := newShell(t, shell.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	p, err := b.Start(ctx, "sleep 1; echo late", backend.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		_ = p.Stop(context.Background())
		t.Fatal("process did not finish")
	}
	if res, _ := p.Result(); res.Output != "late\n" || res.ExitCode != 0 {
		t.Errorf("Result = %+v; cancelling the start context must not stop the process", res)
	}

	if _, err := b.Start(ctx, "echo no", backend.ExecOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Start with a done context = %v, want context.Canceled", err)
	}
}

func TestExecuteContextCancelStopsGroup(t *testing.T) {
	b, root := newShell(t, shell.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	pidFile := filepath.Join(root, "pid")
	start := time.Now()
	_, err := b.Execute(ctx, "echo $$ > pid; sleep 30", backend.ExecOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancelled Execute took %s", elapsed)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	requireGone(t, -firstLinePID(t, string(data)))
}

func TestNewRejectsNegative(t *testing.T) {
	tests := map[string]struct {
		cfg  shell.Config
		want string
	}{
		"Timeout":          {shell.Config{Timeout: -1}, "shell: Timeout is negative"},
		"MaxOutputBytes":   {shell.Config{MaxOutputBytes: -1}, "shell: MaxOutputBytes is negative"},
		"MemoryLimitBytes": {shell.Config{MemoryLimitBytes: -1}, "shell: MemoryLimitBytes is negative"},
		"empty env key":    {shell.Config{Env: map[string]string{"": "x"}}, `shell: env key "" is invalid`},
		"env key with =":   {shell.Config{Env: map[string]string{"A=B": "x"}}, `shell: env key "A=B" is invalid`},
		"env key with NUL": {shell.Config{Env: map[string]string{"A\x00": "x"}}, `shell: env key "A\x00" is invalid`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			b, err := shell.New(t.TempDir(), tt.cfg)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("New error = %v, want %q", err, tt.want)
			}
			if b != nil {
				t.Errorf("New returned a backend with its error")
			}
		})
	}
}

func TestExecRejectsInvalidEnvKey(t *testing.T) {
	b, err := shell.New(t.TempDir(), shell.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "A=B", "A\x00"} {
		want := `shell: env key "` + strings.ReplaceAll(key, "\x00", `\x00`) + `" is invalid`
		opts := backend.ExecOptions{Env: map[string]string{"OK": "1", key: "x"}}
		if res, err := b.Execute(t.Context(), "echo ran", opts); err == nil || err.Error() != want {
			t.Errorf("Execute with env key %q = %+v, %v; want %q", key, res, err, want)
		}
		if p, err := b.Start(t.Context(), "echo ran", opts); err == nil || err.Error() != want {
			if err == nil {
				<-p.Done()
			}
			t.Errorf("Start with env key %q: err = %v, want %q", key, err, want)
		}
	}
}

func TestNewPassesDiskConfig(t *testing.T) {
	root := t.TempDir()
	b, err := shell.New(root, shell.Config{Disk: disk.Config{DisableVirtual: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	abs, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Write(context.Background(), filepath.ToSlash(filepath.Join(abs, "x.txt")), []byte("x")); err != nil {
		t.Fatalf("Write by host path: %v", err)
	}
}
