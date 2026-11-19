//go:build unix

package procgroup

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes the test binary sleep instead of running tests, so the
// group tests spawn nothing but the binary under test. Its value "deaf" also
// ignores SIGTERM.
const helperEnv = "ORX_PROCGROUP_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		if mode == "deaf" {
			signal.Ignore(syscall.SIGTERM)
		}
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startSleeper starts the test binary as a sleeping group leader; the group is
// killed and the leader reaped when the test ends.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	return startHelper(t, "sleep")
}

// startHelper starts the test binary as a group leader in mode.
func startHelper(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
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

// waitInBackground reaps cmd's leader as soon as it exits, as a caller of
// Reap does, so it stops counting as a group member.
func waitInBackground(cmd *exec.Cmd) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return done
}

func TestReapGoneGroup(t *testing.T) {
	cmd := startSleeper(t)
	_ = Kill(cmd.Process)
	<-waitInBackground(cmd)
	start := time.Now()
	if !Reap(cmd.Process, time.Second) {
		t.Fatal("Reap reported an empty group alive")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Reap of an empty group took %v", elapsed)
	}
}

func TestReapTerminatesMembers(t *testing.T) {
	cmd := startSleeper(t)
	reaped := waitInBackground(cmd)
	start := time.Now()
	if !Reap(cmd.Process, 2*time.Second) {
		t.Fatal("group still alive after Reap")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Reap took %v for members that obey SIGTERM", elapsed)
	}
	<-reaped // before the cleanup waits for cmd too
}

func TestReapKillsMembersIgnoringTerm(t *testing.T) {
	cmd := startHelper(t, "deaf")
	reaped := waitInBackground(cmd)
	time.Sleep(100 * time.Millisecond) // let the helper ignore SIGTERM
	grace := 200 * time.Millisecond
	start := time.Now()
	if !Reap(cmd.Process, grace) {
		t.Fatal("group still alive after Reap")
	}
	if elapsed := time.Since(start); elapsed < grace || elapsed > grace+time.Second {
		t.Fatalf("Reap took %v, want SIGKILL after the %v grace", elapsed, grace)
	}
	<-reaped // before the cleanup waits for cmd too
}
