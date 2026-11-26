//go:build unix

package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/internal/procgroup"
)

// processGone reports whether pid no longer runs: it does not exist, or it
// is a zombie (Linux), which an init that does not reap may leave behind.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state follows the parenthesised command name: "pid (comm) S ...".
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func init() {
	// stubborn ignores SIGTERM and keeps running after stdin EOF.
	platformHelpers["stubborn"] = func() { signal.Ignore(syscall.SIGTERM) }
	// escape moves into its parent's process group, leaving the group it
	// leads empty, and ignores SIGTERM, so only a signal sent to the process
	// itself can stop it.
	platformHelpers["escape"] = func() {
		if pgid, err := syscall.Getpgid(os.Getppid()); err == nil {
			_ = syscall.Setpgid(0, pgid)
		}
		signal.Ignore(syscall.SIGTERM)
	}
}

func TestCloseKillsServerThatLeftItsGroup(t *testing.T) {
	c := clientFor(t, startHelper(t, "escape", StdioConfig{}))
	pid, err := strconv.Atoi(callText(t, c, "pid", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if pgid, _ := syscall.Getpgid(pid); pgid == pid {
		t.Fatal("helper did not leave its group")
	}

	closed := make(chan error, 1)
	go func() { closed <- c.Close(context.Background()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("Close hung on a server outside its process group")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("server %d still running after Close", pid)
	}
}

func TestCloseKillsStubbornServer(t *testing.T) {
	tr := startHelper(t, "stubborn", StdioConfig{CloseGrace: 300 * time.Millisecond})
	c := clientFor(t, tr)
	pid, err := strconv.Atoi(callText(t, c, "pid", `{}`))
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 1500*time.Millisecond {
		t.Fatalf("Close took %v, want < 1.5s", elapsed)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("server %d still running after Close", pid)
	}
}

func TestCloseKillsGrandchild(t *testing.T) {
	c := clientFor(t, startHelper(t, "spawn", StdioConfig{}))
	pid, err := strconv.Atoi(callText(t, c, "spawn", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) }) // only if the test failed
	if processGone(pid) {
		t.Fatal("grandchild not running before Close")
	}

	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still running 2s after Close", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExitedServerLeftoversKilled(t *testing.T) {
	c := clientFor(t, startHelper(t, "spawn", StdioConfig{}))
	pid, err := strconv.Atoi(callText(t, c, "spawn", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) }) // only if the test failed

	if _, err := c.CallTool(context.Background(), "quit", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("quit: err = %v, want ErrClosed", err)
	}
	// No Close: the server exiting on its own takes its descendants along.
	deadline := time.Now().Add(2 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still running 2s after the server exited", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// standIn returns a transport whose "server" is a helper leading its own
// group, standing in for a group whose pgid the kernel has handed out again.
// The transport never waits for it; the test kills and reaps it at the end.
func standIn(t *testing.T, mode string) *stdioTransport {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, "GORACE=atexit_sleep_ms=0")
	procgroup.Setup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, stdin, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return &stdioTransport{cmd: cmd, stdin: stdin, grace: 200 * time.Millisecond,
		exited: make(chan struct{}), reaped: make(chan struct{})}
}

// requireAlive fails when cmd's process has exited.
func requireAlive(t *testing.T, cmd *exec.Cmd, what string) {
	t.Helper()
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("%s: process %d is gone (%v)", what, cmd.Process.Pid, err)
	}
	var status syscall.WaitStatus
	if pid, _ := syscall.Wait4(cmd.Process.Pid, &status, syscall.WNOHANG, nil); pid != 0 {
		t.Fatalf("%s: process %d exited (%v)", what, cmd.Process.Pid, status)
	}
}

func TestStopSkipsSignalsOnceGroupGone(t *testing.T) {
	tr := standIn(t, "sleep")
	tr.groupGone.Store(true)

	if err := tr.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	requireAlive(t, tr.cmd, "stop after the group was gone")
}

func TestDelayedKillSkippedOnceGroupGone(t *testing.T) {
	// stubborn ignores SIGTERM, so only SIGKILL would end it.
	tr := standIn(t, "stubborn")
	time.Sleep(100 * time.Millisecond) // let it ignore SIGTERM

	stopped := make(chan error, 1)
	go func() { stopped <- tr.stop() }()
	// The group is seen gone after SIGTERM, before the SIGKILL step; its pgid
	// may belong to someone else by then.
	time.Sleep(tr.grace + tr.grace/2)
	tr.groupGone.Store(true)
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	requireAlive(t, tr.cmd, "SIGKILL after the group was gone")
}

func TestClientCloseContextExpired(t *testing.T) {
	baseline := runtime.NumGoroutine()
	tr := startHelper(t, "stubborn", StdioConfig{CloseGrace: 200 * time.Millisecond})
	c := clientFor(t, tr)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("Close took %v, want < 1s", elapsed)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done not closed after Close returned nil")
	}

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d, baseline %d\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
