//go:build unix

package procgroup

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes the test binary sleep instead of running tests, so the
// group tests spawn nothing but the binary under test.
const helperEnv = "ORX_PROCGROUP_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startSleeper starts the test binary as a sleeping group leader; the group is
// killed and the leader reaped when the test ends.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	Setup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

func TestSetupMakesGroupLeader(t *testing.T) {
	cmd := startSleeper(t)
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if pgid != cmd.Process.Pid {
		t.Fatalf("pgid = %d, want the leader's pid %d", pgid, cmd.Process.Pid)
	}
}

func TestTerminateEmptiesGroup(t *testing.T) {
	cmd := startSleeper(t)
	if !Alive(cmd.Process) {
		t.Fatal("group of a running leader is not alive")
	}
	if err := Terminate(cmd.Process); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	_ = cmd.Wait() // reap the leader, or it lingers as a group member
	if !WaitGone(cmd.Process, 2*time.Second) {
		t.Fatal("group still alive after SIGTERM")
	}
	if Alive(cmd.Process) {
		t.Fatal("Alive reports an empty group")
	}
}

func TestSignalsToGoneGroupAreNotErrors(t *testing.T) {
	cmd := startSleeper(t)
	if err := Kill(cmd.Process); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = cmd.Wait()
	if !WaitGone(cmd.Process, 2*time.Second) {
		t.Fatal("group still alive after SIGKILL")
	}
	if err := Terminate(cmd.Process); err != nil {
		t.Errorf("Terminate on a gone group: %v", err)
	}
	if err := Kill(cmd.Process); err != nil {
		t.Errorf("Kill on a gone group: %v", err)
	}
}

func TestWaitGoneTimesOut(t *testing.T) {
	cmd := startSleeper(t)
	start := time.Now()
	if WaitGone(cmd.Process, 100*time.Millisecond) {
		t.Fatal("WaitGone reported a live group gone")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("WaitGone took %v for a 100ms bound", elapsed)
	}
}
