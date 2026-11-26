//go:build unix

package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Setup makes cmd, once started, the leader of a new process group, so that
// signalling the group reaches every process the command spawns.
func Setup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Terminate asks every process in proc's group to exit (SIGTERM). A group
// that is already gone is not an error.
func Terminate(proc *os.Process) error {
	return signalGroup(proc, syscall.SIGTERM)
}

// Kill kills every process in proc's group (SIGKILL). A group that is
// already gone is not an error.
func Kill(proc *os.Process) error {
	return signalGroup(proc, syscall.SIGKILL)
}

func signalGroup(proc *os.Process, sig syscall.Signal) error {
	if err := syscall.Kill(-proc.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// alive reports whether any process of proc's group still exists. EPERM
// means a member exists that we may not signal, which still counts as alive.
//
// Once the leader has been waited for and alive has returned false, the group
// id may be handed out again, so callers must stop signalling it.
func alive(proc *os.Process) bool {
	err := syscall.Kill(-proc.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
