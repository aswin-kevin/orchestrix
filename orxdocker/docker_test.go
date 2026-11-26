package orxdocker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/sandbox"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
	"github.com/aswin-kevin/orchestrix/permission"
)

// The tests below talk to a real Docker daemon. They run only when
// ORX_TEST_DOCKER=1, and then fail (rather than skip) if the daemon cannot
// be reached. ORX_TEST_DOCKER_IMAGE overrides the image.

var scopeSeq atomic.Int64

// newTestManager returns a Manager with a scope of its own, whose
// containers are all removed when the test ends.
func newTestManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	if os.Getenv("ORX_TEST_DOCKER") != "1" {
		t.Skip("set ORX_TEST_DOCKER=1 to run against a Docker daemon")
	}
	if opts.Scope == "" {
		opts.Scope = fmt.Sprintf("orxtest-%d-%d", os.Getpid(), scopeSeq.Add(1))
	}
	if opts.Image == "" {
		opts.Image = os.Getenv("ORX_TEST_DOCKER_IMAGE")
	}
	if opts.IdleTTL == 0 {
		opts.IdleTTL = -1
	}
	m, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ping(t.Context()); err != nil {
		t.Fatalf("Docker daemon unreachable: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if _, err := m.Reap(context.Background(), ReapOptions{}); err != nil {
			t.Errorf("Reap: %v", err)
		}
	})
	return m
}

// run executes cmd in the thread's container and fails the test on error.
func run(t *testing.T, e *Executor, cmd string, opts backend.ExecOptions) backend.ExecResult {
	t.Helper()
	if opts.Timeout == 0 {
		opts.Timeout = time.Minute
	}
	res, err := e.Execute(t.Context(), cmd, opts)
	if err != nil {
		t.Fatalf("Execute(%q): %v", cmd, err)
	}
	return res
}

// processes lists the command lines running in the container, minus the
// listing itself: its wrapper, ps, and the backstop guarding ps.
func processes(t *testing.T, e *Executor) string {
	t.Helper()
	const listing = "ps -o pid,pgid,ppid,args"
	res := run(t, e, listing, backend.ExecOptions{})
	lines := strings.Split(res.Output, "\n")
	own := map[string]bool{} // pids and group ids of the listing's processes
	for _, line := range lines {
		if f := strings.Fields(line); len(f) > 1 && (strings.Contains(line, listing) || strings.Contains(line, b64(listing))) {
			own[f[0]] = true
		}
	}
	for _, line := range lines {
		if f := strings.Fields(line); len(f) > 1 && strings.Contains(line, "orx-backstop") && own[f[len(f)-1]] {
			own[f[1]] = true
		}
	}
	var keep []string
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) > 3 && (own[f[0]] || own[f[1]]) || strings.Contains(line, "orx-signal") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// managedContainers lists the containers of m's scope.
func managedContainers(t *testing.T, m *Manager, extra ...string) []containerSummary {
	t.Helper()
	list, err := m.c.listContainers(t.Context(), m.scopeFilters(extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestDockerConformance(t *testing.T) {
	m := newTestManager(t, Options{})
	e := m.Executor("conformance")
	var n atomic.Int64
	backendtest.RunWith(t, func(t *testing.T) backend.Backend {
		b, err := sandbox.NewStarter(e, sandbox.Options{Root: fmt.Sprintf("/workspace/case%d", n.Add(1))})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}, backendtest.Options{ModTimeResolution: time.Second})
}

// hostileNames matches backend/sandbox's list: names built to break out of a
// badly quoted shell word.
var hostileNames = []string{
	"/a'$(touch pwned)'",
	"/b`touch pwned`.txt",
	"/c d/-rf",
	"/e\nf.txt",
	`/g"$(touch pwned)".txt`,
	"/h;touch pwned;.txt",
	"/-n",
	"/i *.txt",
	"/j?.txt",
	"/k[ab].txt",
	"/l\\n\\0 %s %d.txt",
	"/m\t$HOME'\"'.txt",
	"/n'' && touch pwned #.txt",
}

// nonUTF8Name is a file name that is not valid UTF-8.
const nonUTF8Name = "/bad-\xff\xfe.txt"

func TestDockerHostileNames(t *testing.T) {
	m := newTestManager(t, Options{})
	b := m.Backend("hostile")
	ctx := t.Context()
	for i, name := range hostileNames {
		content := fmt.Sprintf("content of file %d", i)
		if err := b.Write(ctx, name, []byte(content)); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
		res, err := b.Read(ctx, name, 0, 0)
		if err != nil {
			t.Fatalf("Read(%q): %v", name, err)
		}
		if got := strings.Join(res.Lines, "\n"); got != content {
			t.Fatalf("Read(%q) = %q", name, got)
		}
		if _, err := b.Edit(ctx, name, "content", "CONTENT", false); err != nil {
			t.Fatalf("Edit(%q): %v", name, err)
		}
	}
	infos, err := b.Glob(ctx, "**", "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != len(hostileNames) {
		t.Fatalf("Glob found %d files, want %d", len(infos), len(hostileNames))
	}
	matches, err := b.Grep(ctx, "CONTENT of", backend.GrepOptions{Mode: backend.GrepFiles, Literal: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != len(hostileNames) {
		t.Fatalf("Grep found %d files, want %d", len(matches), len(hostileNames))
	}
	for _, name := range hostileNames {
		if err := b.Delete(ctx, name); err != nil {
			t.Fatalf("Delete(%q): %v", name, err)
		}
	}
	e := m.Executor("hostile")
	if out := run(t, e, "find / -xdev -name pwned 2>/dev/null; find /workspace /tmp -name pwned", backend.ExecOptions{}).Output; strings.TrimSpace(out) != "" {
		t.Fatalf("injected command ran: %q", out)
	}
}

// TestDockerNonUTF8Name checks that a name that is not valid UTF-8 reaches
// the container intact although the Engine API speaks JSON. Userlands whose
// tools refuse such arguments (Rust coreutils) must fail with the sandbox's
// explicit error instead.
func TestDockerNonUTF8Name(t *testing.T) {
	m := newTestManager(t, Options{})
	b := m.Backend("bytes")
	ctx := t.Context()
	if err := b.Write(ctx, nonUTF8Name, []byte("bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	res, err := b.Read(ctx, nonUTF8Name, 0, 0)
	if err != nil && strings.Contains(err.Error(), "cannot handle non-UTF-8 file names") {
		t.Skipf("this image's userland refuses non-UTF-8 arguments: %v", err)
	}
	if err != nil || strings.Join(res.Lines, "") != "bytes" {
		t.Fatalf("Read = %q, %v", res.Lines, err)
	}
	infos, err := b.Ls(ctx, "/")
	if err != nil || len(infos) != 1 || infos[0].Path != nonUTF8Name {
		t.Fatalf("Ls = %+v, %v", infos, err)
	}
	if err := b.Delete(ctx, nonUTF8Name); err != nil {
		t.Fatal(err)
	}
}

func TestDockerNoHostEnvironment(t *testing.T) {
	t.Setenv("ORX_HOST_CANARY", "host-secret-value")
	m := newTestManager(t, Options{Env: []string{"FROM_OPTIONS=1", "SHARED=options"}})
	e := m.Executor("env")
	res := run(t, e, "env", backend.ExecOptions{Env: []string{"FROM_CALL=1", "SHARED=call"}})
	if strings.Contains(res.Output, "host-secret-value") || strings.Contains(res.Output, "ORX_HOST_CANARY") {
		t.Fatalf("host environment leaked: %q", res.Output)
	}
	for _, want := range []string{"FROM_OPTIONS=1", "FROM_CALL=1", "SHARED=call"} {
		if !strings.Contains(res.Output, want+"\n") {
			t.Errorf("env lacks %s: %q", want, res.Output)
		}
	}
	if strings.Contains(res.Output, "SHARED=options") {
		t.Errorf("ExecOptions.Env did not override Options.Env: %q", res.Output)
	}
}

func TestDockerNoNetwork(t *testing.T) {
	m := newTestManager(t, Options{})
	e := m.Executor("net")
	if got := strings.TrimSpace(run(t, e, "ls /sys/class/net", backend.ExecOptions{}).Output); got != "lo" {
		t.Errorf("network interfaces = %q, want only lo", got)
	}
	for _, target := range []string{"http://169.254.169.254/latest/meta-data/", "http://1.1.1.1/", "http://example.com/"} {
		res := run(t, e, "wget -q -T 3 -O- "+target, backend.ExecOptions{Timeout: 20 * time.Second})
		if res.ExitCode == 0 {
			t.Errorf("reached %s: %q", target, res.Output)
		}
	}
}

func TestDockerHardening(t *testing.T) {
	m := newTestManager(t, Options{})
	e := m.Executor("hardening")
	if got := strings.TrimSpace(run(t, e, "id -u; id -g", backend.ExecOptions{}).Output); got != "65534\n65534" {
		t.Errorf("ids = %q", got)
	}
	status := run(t, e, "cat /proc/self/status", backend.ExecOptions{}).Output
	for _, want := range []string{"CapEff:\t0000000000000000", "CapPrm:\t0000000000000000", "CapBnd:\t0000000000000000", "NoNewPrivs:\t1", "Seccomp:\t2"} {
		if !strings.Contains(status, want) {
			t.Errorf("/proc/self/status lacks %q", want)
		}
	}
	for _, target := range []string{"/x", "/etc/x", "/usr/bin/x", "/var/x", "/home/x", "/root/x"} {
		if res := run(t, e, "echo pwned > "+target, backend.ExecOptions{}); res.ExitCode == 0 {
			t.Errorf("wrote %s", target)
		}
	}
	if res := run(t, e, "echo ok > /workspace/x && echo ok > /tmp/x && cat /workspace/x /tmp/x", backend.ExecOptions{}); res.ExitCode != 0 || res.Output != "ok\nok\n" {
		t.Errorf("writing the workspace and /tmp: %+v", res)
	}
	if got := strings.TrimSpace(run(t, e, "cat /sys/fs/cgroup/pids.max", backend.ExecOptions{}).Output); got != strconv.Itoa(DefaultPidsLimit) {
		t.Errorf("pids.max = %q", got)
	}
}

func TestDockerForkBombIsContained(t *testing.T) {
	m := newTestManager(t, Options{PidsLimit: 64})
	e := m.Executor("forkbomb")
	start := time.Now()
	// The shell stays in the foreground so the bomb runs until the timeout.
	res, err := e.Execute(t.Context(), "bomb() { bomb | bomb & }; bomb; sleep 30", backend.ExecOptions{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fork bomb: %s, exit %d, timed out %v", time.Since(start).Round(time.Millisecond), res.ExitCode, res.TimedOut)
	if !res.TimedOut {
		t.Errorf("fork bomb ended before its timeout: %+v", res)
	}
	// The container must recover: the bomb's group is killed and new
	// commands run.
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := e.Execute(t.Context(), "echo alive", backend.ExecOptions{Timeout: 10 * time.Second})
		if err == nil && strings.TrimSpace(res.Output) == "alive" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("container did not recover: %+v, %v", res, err)
		}
		time.Sleep(time.Second)
	}
	// The limit was hit: pids.events counts refused forks.
	events := run(t, e, "cat /sys/fs/cgroup/pids.events", backend.ExecOptions{}).Output
	t.Logf("pids.events: %s", strings.TrimSpace(events))
	if strings.Contains(events, "max 0") || !strings.Contains(events, "max ") {
		t.Errorf("the pids limit was never reached: %q", events)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		ps := processes(t, e)
		if !strings.Contains(ps, "bomb") && strings.Count(ps, "\n") < 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fork bomb processes left: %s", ps)
		}
		time.Sleep(time.Second)
	}
}

func TestDockerTimeoutKillsProcess(t *testing.T) {
	m := newTestManager(t, Options{})
	e := m.Executor("timeout")
	start := time.Now()
	res, err := e.Execute(t.Context(), "sleep 600", backend.ExecOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("result = %+v", res)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if ps := processes(t, e); strings.Contains(ps, "sleep 600") || strings.Contains(ps, "orx-backstop") {
		t.Fatalf("processes left after the timeout:\n%s", ps)
	}

	// A command ignoring SIGTERM is killed after the grace period.
	res, err = e.Execute(t.Context(), "trap '' TERM; sleep 601", backend.ExecOptions{Timeout: time.Second})
	if err != nil || !res.TimedOut {
		t.Fatalf("TERM-proof command: %+v, %v", res, err)
	}
	if ps := processes(t, e); strings.Contains(ps, "sleep 601") {
		t.Fatalf("TERM-proof command survived:\n%s", ps)
	}

	// A cancelled context stops the command too.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := e.Execute(ctx, "sleep 602", backend.ExecOptions{Timeout: time.Hour}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled Execute: err = %v", err)
	}
	if ps := processes(t, e); strings.Contains(ps, "sleep 602") {
		t.Fatalf("command survived its cancelled context:\n%s", ps)
	}

	// What a command leaves in its group dies with it.
	if res := run(t, e, "sleep 603 & echo started", backend.ExecOptions{}); res.Output != "started\n" {
		t.Fatalf("background child: %+v", res)
	}
	if ps := processes(t, e); strings.Contains(ps, "sleep 603") {
		t.Fatalf("background child survived:\n%s", ps)
	}
}

// A command that removes its pid file and kills the backstop (and with it
// the wrapper, whose script names the backstop) defeats the in-container
// kill; its kill cannot be confirmed, so the container is removed instead
// and the next command gets a new one.
func TestDockerHostileCommandCannotOutliveTimeout(t *testing.T) {
	m := newTestManager(t, Options{})
	e := m.Executor("hostile")
	before, err := e.ContainerID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := e.Execute(t.Context(),
		"sleep 1; rm -f /tmp/.orx-*; pkill -KILL -f 'orx-backst[o]p'; trap '' TERM; sleep 600",
		backend.ExecOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second+hardGrace+5*time.Second {
		t.Fatalf("hostile command held Execute for %s", elapsed)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("result = %+v", res)
	}
	after, err := e.ContainerID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("the hostile command's container was kept")
	}
	if res := run(t, e, "echo fresh", backend.ExecOptions{}); res.Output != "fresh\n" {
		t.Fatalf("next command: %+v", res)
	}
}

func TestDockerExecOptions(t *testing.T) {
	m := newTestManager(t, Options{})
	e := m.Executor("opts")
	if res := run(t, e, "echo out; echo err >&2; exit 7", backend.ExecOptions{}); res.ExitCode != 7 || res.Output != "out\nerr\n" {
		t.Errorf("exit and output: %+v", res)
	}
	if res := run(t, e, "pwd", backend.ExecOptions{}); res.Output != "/workspace\n" {
		t.Errorf("default dir: %q", res.Output)
	}
	if res := run(t, e, "pwd", backend.ExecOptions{Dir: "/tmp"}); res.Output != "/tmp\n" {
		t.Errorf("Dir: %q", res.Output)
	}
	if res := run(t, e, "echo ran", backend.ExecOptions{Dir: "/missing"}); res.ExitCode == 0 || strings.Contains(res.Output, "ran") {
		t.Errorf("missing Dir: %+v", res)
	}
	if _, err := e.Execute(t.Context(), "true", backend.ExecOptions{Dir: "relative"}); err == nil {
		t.Error("relative Dir accepted")
	}
	res := run(t, e, "head -c 3000000 /dev/zero | tr '\\0' x; echo; echo the-end", backend.ExecOptions{MaxOutputBytes: 1000})
	if !res.Truncated || len(res.Output) > 1100 || !strings.HasSuffix(res.Output, "the-end\n") || !strings.HasPrefix(res.Output, "xxx") {
		t.Errorf("capped output: truncated %v, %d bytes, tail %q", res.Truncated, len(res.Output), res.Output[max(0, len(res.Output)-20):])
	}
	if res := run(t, e, "printf 'a\\000b\\377'", backend.ExecOptions{}); res.Output != "a\x00b\xff" {
		t.Errorf("binary output = %q", res.Output)
	}
}

func TestDockerStartAndStop(t *testing.T) {
	m := newTestManager(t, Options{})
	e := m.Executor("start")
	p, err := e.Start(t.Context(), "echo begin; sleep 604", backend.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(p.Output(), "begin") {
		if time.Now().After(deadline) {
			t.Fatalf("no output while running: %q", p.Output())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := p.Result(); ok {
		t.Fatal("result before the command ended")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not end the command")
	}
	if res, _ := p.Result(); res.ExitCode == 0 || res.Output != "begin\n" {
		t.Errorf("stopped result = %+v", res)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if ps := processes(t, e); strings.Contains(ps, "sleep 604") {
		t.Fatalf("stopped command survived:\n%s", ps)
	}
}

func TestDockerWithTasks(t *testing.T) {
	m := newTestManager(t, Options{})
	tm := tasks.New(tasks.Config{})
	defer tm.Shutdown()
	call := func(id, name string, in map[string]any) llmtest.Turn {
		return llmtest.Turn{ToolCalls: []llmtest.Call{{ID: id, Name: name, Input: in}}}
	}
	p := llmtest.New(
		call("e1", "execute", map[string]any{"command": "pwd; echo started; exec sleep 605", "run_in_background": true}),
		call("m1", tasks.ToolMonitor, map[string]any{"task_id": "bg_1", "pattern": "^started$", "timeout_seconds": 30}),
		call("o1", tasks.ToolTaskOutput, map[string]any{"task_id": "bg_1"}),
		call("x1", tasks.ToolTaskStop, map[string]any{"task_id": "bg_1"}),
		call("o2", tasks.ToolTaskOutput, map[string]any{"task_id": "bg_1", "block": true, "timeout_seconds": 30}),
		llmtest.Turn{Text: "ok"},
	)
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithBackendFactory(m.Backend), orchestrix.WithTasks(tm),
		orchestrix.WithPermissionMode(permission.ModeBypass))
	if err != nil {
		t.Fatal(err)
	}
	for ev := range a.Run(t.Context(), agent.Input{ThreadID: "tasks", Messages: []llm.Message{llm.UserText("go")}}) {
		if ev.Final != nil && ev.Final.Err != nil {
			t.Fatal(ev.Final.Err)
		}
	}
	if got := toolResult(p, "o1"); !strings.Contains(got, "running") || !strings.Contains(got, "/workspace") {
		t.Errorf("task_output while running = %q", got)
	}
	if got := toolResult(p, "o2"); !strings.Contains(got, "stopped") {
		t.Errorf("task_output after stop = %q", got)
	}
	if ps := processes(t, m.Executor("tasks")); strings.Contains(ps, "sleep 605") {
		t.Fatalf("stopped job survived:\n%s", ps)
	}
}

// toolResult returns the text of the tool result answering call id.
func toolResult(p *llmtest.Scripted, id string) string {
	var parts []string
	for _, req := range p.Requests {
		for _, msg := range req.Messages {
			for _, b := range msg.Content {
				if b.Type == llm.BlockToolResult && b.ToolUseID == id {
					for _, c := range b.Content {
						parts = append(parts, c.Text)
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

func TestDockerContainerPerThread(t *testing.T) {
	m := newTestManager(t, Options{})
	ctx := t.Context()
	a, b := m.Backend("thread-a"), m.Backend("thread-b")
	if err := a.Write(ctx, "/f.txt", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(ctx, "/f.txt", 0, 0); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("thread b sees thread a's file: %v", err)
	}
	if res, err := m.Backend("thread-a").Read(ctx, "/f.txt", 0, 0); err != nil || res.Lines[0] != "a" {
		t.Fatalf("thread a's container was not reused: %v", err)
	}
	idA, _ := m.Executor("thread-a").ContainerID(ctx)
	idB, _ := m.Executor("thread-b").ContainerID(ctx)
	if idA == "" || idA == idB {
		t.Fatalf("container ids %q and %q", idA, idB)
	}
	list := managedContainers(t, m, LabelThread+"=thread-a")
	if len(list) != 1 || list[0].ID != idA {
		t.Fatalf("containers of thread-a = %+v", list)
	}
	l := list[0].Labels
	if _, err := time.Parse(time.RFC3339, l[LabelCreated]); err != nil || l[LabelThreadHash] != threadHash("thread-a") {
		t.Errorf("labels = %v", l)
	}

	if err := m.Delete(ctx, "thread-a"); err != nil {
		t.Fatal(err)
	}
	if list := managedContainers(t, m, LabelThread+"=thread-a"); len(list) != 0 {
		t.Fatalf("Delete left %+v", list)
	}
	if _, err := m.Backend("thread-a").Read(ctx, "/f.txt", 0, 0); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("deleted thread's file survived: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if list := managedContainers(t, m); len(list) != 0 {
		t.Fatalf("Close left %d containers", len(list))
	}
	if _, err := b.Read(ctx, "/f.txt", 0, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
}

func TestDockerIdleTTL(t *testing.T) {
	m := newTestManager(t, Options{IdleTTL: 2 * time.Second})
	e := m.Executor("idle")
	run(t, e, "echo hi > /workspace/f", backend.ExecOptions{})
	first, _ := e.ContainerID(t.Context())
	deadline := time.Now().Add(20 * time.Second)
	for len(managedContainers(t, m)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle container was not removed")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if res := run(t, m.Executor("idle"), "cat /workspace/f", backend.ExecOptions{}); res.ExitCode == 0 {
		t.Fatalf("file survived the idle removal: %q", res.Output)
	}
	if again, _ := m.Executor("idle").ContainerID(t.Context()); again == first {
		t.Fatal("container id reused after removal")
	}

	// A running command keeps its container alive past the TTL.
	p, err := m.Executor("busy").Start(t.Context(), "sleep 6; echo finished", backend.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	if res, _ := p.Result(); res.Output != "finished\n" || res.ExitCode != 0 {
		t.Fatalf("long command under idle TTL: %+v", res)
	}
}

func TestDockerReapAndAdopt(t *testing.T) {
	m1 := newTestManager(t, Options{})
	ctx := t.Context()
	keepID, err := m1.Executor("keep").ContainerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	goneID, err := m1.Executor("gone").ContainerID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A second Manager on the same scope (a restarted process) reuses the
	// running container of a thread.
	m2 := newTestManager(t, Options{Scope: m1.opts.Scope})
	if id, err := m2.Executor("keep").ContainerID(ctx); err != nil || id != keepID {
		t.Fatalf("adopted %q, %v; want %q", id, err, keepID)
	}
	// Containers younger than OlderThan stay.
	if removed, err := m2.Reap(ctx, ReapOptions{OlderThan: time.Hour}); err != nil || len(removed) != 0 {
		t.Fatalf("Reap(OlderThan 1h) = %v, %v", removed, err)
	}
	removed, err := m2.Reap(ctx, ReapOptions{Keep: func(thread string) bool { return thread == "keep" }})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{goneID}) {
		t.Fatalf("Reap removed %v, want [%s]", removed, goneID)
	}
	if list := managedContainers(t, m1); len(list) != 1 || list[0].ID != keepID {
		t.Fatalf("after Reap: %+v", list)
	}
	// The first Manager notices that its container is gone.
	if _, err := m1.Executor("gone").Execute(ctx, "true", backend.ExecOptions{}); !errors.Is(err, ErrContainerGone) {
		t.Fatalf("Execute in a reaped container: %v", err)
	}
	if res := run(t, m1.Executor("gone"), "echo back", backend.ExecOptions{}); res.Output != "back\n" {
		t.Fatalf("after recreation: %+v", res)
	}
}
