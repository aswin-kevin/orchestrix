package procgroup

import (
	"os"
	"time"
)

// pollInterval is how often waitGone checks a dying group for members.
const pollInterval = 10 * time.Millisecond

// waitGone polls until proc's group has no members or within elapses,
// reporting whether the group is gone. A leader that has exited but not been
// waited for still counts as a member.
func waitGone(proc *os.Process, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for alive(proc) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
	return true
}

// Reap kills whatever is left in proc's group once the leader has exited:
// SIGTERM, then SIGKILL for members that survive grace. It reports whether
// the group is gone; it gives up after a further grace, since a killed member
// can linger as a zombie under an init that does not reap. The leader must be
// waited for concurrently or beforehand, or it counts as a member.
//
// Once Reap has returned true the group id may be handed out again: the
// caller must latch that and never signal the group afterwards.
func Reap(proc *os.Process, grace time.Duration) bool {
	if !alive(proc) {
		return true
	}
	_ = Terminate(proc)
	if waitGone(proc, grace) {
		return true
	}
	_ = Kill(proc)
	return waitGone(proc, grace)
}
