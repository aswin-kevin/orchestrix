//go:build !unix

package procgroup

import (
	"errors"
	"os"
	"os/exec"
)

// Setup does nothing: there are no process groups to join.
func Setup(*exec.Cmd) {}

// Terminate kills proc itself; its descendants are not reached.
func Terminate(proc *os.Process) error { return kill(proc) }

// Kill kills proc itself; its descendants are not reached.
func Kill(proc *os.Process) error { return kill(proc) }

func kill(proc *os.Process) error {
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// Alive is false: without a group there is nothing left to reap once the
// process has exited.
func Alive(*os.Process) bool { return false }
