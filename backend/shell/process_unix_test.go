//go:build unix

package shell

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/internal/procgroup"
)

// startGroupLeader starts command as the leader of its own process group,
// standing in for a group whose pgid the kernel has handed out again. It is
// killed and reaped when the test ends.
func startGroupLeader(t *testing.T, command string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(Shell, "-c", command)
	procgroup.Setup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
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
	cmd := startGroupLeader(t, "sleep 1000")
	p := &process{cmd: cmd, done: make(chan struct{})}
	p.groupGone.Store(true)

	if err := p.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	requireAlive(t, cmd, "stop after the group was gone")
}

func TestDelayedKillSkippedOnceGroupGone(t *testing.T) {
	// The ignored SIGTERM is inherited by sleep, so only SIGKILL would end it.
	cmd := startGroupLeader(t, "trap '' TERM; sleep 1000")
	p := &process{cmd: cmd, done: make(chan struct{})}
	time.Sleep(100 * time.Millisecond) // let the trap take effect

	if err := p.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// The group is seen gone before the delayed SIGKILL fires; its pgid may
	// belong to someone else by then.
	p.groupGone.Store(true)
	time.Sleep(DefaultKillGrace + 300*time.Millisecond)
	requireAlive(t, cmd, "delayed SIGKILL after the group was gone")
}

func TestStopTakesCtx(t *testing.T) {
	b, err := New(t.TempDir(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := b.Start(t.Context(), "sleep 5", backend.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Stop is a local signal: a cancelled ctx must neither fail it nor keep
	// it from sending the signal.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop(cancelled ctx) = %v, want nil", err)
	}
	select {
	case <-p.Done():
	case <-time.After(3 * backend.DefaultKillGrace):
		t.Fatal("Done not closed after Stop")
	}
}
