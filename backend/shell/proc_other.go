//go:build !unix

package shell

import (
	"errors"
	"os"
	"os/exec"
)

// On platforms without POSIX process groups (Windows) execution is
// best-effort: only the shell process itself is killed, so processes it
// spawned may outlive a timeout or Stop.

func startInGroup(*exec.Cmd) {}

func terminateGroup(proc *os.Process) error { return killProcess(proc) }

func killGroup(proc *os.Process) error { return killProcess(proc) }

func killProcess(proc *os.Process) error {
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// groupAlive is false: without a group there is nothing left to reap once the
// shell has exited.
func groupAlive(*os.Process) bool { return false }
