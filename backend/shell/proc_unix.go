//go:build unix

package shell

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// startInGroup makes the shell the leader of a new process group, so that
// signalling the group reaches every process the command spawns.
func startInGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup asks every process in proc's group to exit. A group that is
// already gone is not an error.
func terminateGroup(proc *os.Process) error {
	return signalGroup(proc, syscall.SIGTERM)
}

// killGroup kills every process in proc's group.
func killGroup(proc *os.Process) error {
	return signalGroup(proc, syscall.SIGKILL)
}

func signalGroup(proc *os.Process, sig syscall.Signal) error {
	if err := syscall.Kill(-proc.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// groupAlive reports whether any process of proc's group still exists. EPERM
// means a member exists that we may not signal, which still counts as alive.
func groupAlive(proc *os.Process) bool {
	err := syscall.Kill(-proc.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
