//go:build unix

package mcp

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	go func() { closed <- c.Close() }()
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
	if err := c.Close(); err != nil {
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

	if err := c.Close(); err != nil {
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
