// Package procgroup runs a child process as the leader of its own process
// group, so that everything the child spawns can be signalled and reaped
// together. It is shared by the shell backend and the MCP stdio transport.
//
// On platforms without POSIX process groups (Windows) it is best-effort: only
// the started process itself is killed, so processes it spawned may outlive
// it, and Alive is always false.
package procgroup

import (
	"os"
	"time"
)

// PollInterval is how often WaitGone checks a dying group for members.
const PollInterval = 10 * time.Millisecond

// WaitGone polls until proc's group has no members or within elapses,
// reporting whether the group is gone. A leader that has exited but not been
// waited for still counts as a member.
func WaitGone(proc *os.Process, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for Alive(proc) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(PollInterval)
	}
	return true
}
