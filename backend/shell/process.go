package shell

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/internal/procgroup"
)

// memoryNotice is appended to the output of a command killed by the memory
// watchdog; the verb is the limit in bytes.
const memoryNotice = "\n[killed: memory limit of %d bytes exceeded]"

// process is a started command; it implements backend.Process.
type process struct {
	cmd      *exec.Cmd
	out      *capture
	outRead  *os.File      // read end of the pipe shared by stdout and stderr
	copied   chan struct{} // closed when the output copier has returned
	memLimit int64

	timedOut  atomic.Bool
	memKilled atomic.Bool
	// groupGone is set once reapGroup has seen the process group empty.
	// From then on the group id may be reused by unrelated processes, so
	// nothing signals it any more.
	groupGone atomic.Bool

	stopOnce sync.Once
	stopErr  error

	done   chan struct{}
	result backend.ExecResult // written before done is closed
}

var _ backend.Process = (*process)(nil)

// startProcess starts cmd in its own process group with stdout and stderr
// feeding one capture of maxOutput bytes, and stops it after timeout (and, on
// Linux, when its group's memory exceeds a non-zero memLimit).
//
// The output goes through a pipe the backend owns rather than one exec
// creates: cmd.Wait then returns as soon as the shell exits, even if a
// background grandchild still holds the pipe, which lets the backend notice
// the exit and kill the leftovers instead of waiting on them.
func startProcess(cmd *exec.Cmd, maxOutput int, timeout time.Duration, memLimit int64) (*process, error) {
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("shell: output pipe: %w", err)
	}
	cmd.Stdout, cmd.Stderr = outWrite, outWrite
	procgroup.Setup(cmd)
	startErr := cmd.Start()
	// The child holds its own copy; ours would keep the pipe from ever
	// reaching EOF.
	_ = outWrite.Close()
	if startErr != nil {
		_ = outRead.Close()
		return nil, fmt.Errorf("shell: start: %w", startErr)
	}
	p := &process{
		cmd:      cmd,
		out:      newCapture(maxOutput),
		outRead:  outRead,
		copied:   make(chan struct{}),
		memLimit: memLimit,
		done:     make(chan struct{}),
	}
	go p.copyOutput()
	timer := time.AfterFunc(timeout, func() {
		p.timedOut.Store(true)
		_ = p.stop()
	})
	if memLimit > 0 {
		go watchMemory(cmd.Process.Pid, memLimit, p.done, func() {
			p.memKilled.Store(true)
			_ = p.stop()
		})
	}
	go p.wait(timer)
	return p, nil
}

// Done is closed once the command has finished and its group is gone.
func (p *process) Done() <-chan struct{} { return p.done }

// Result returns the final result once Done is closed.
func (p *process) Result() (backend.ExecResult, bool) {
	select {
	case <-p.done:
		return p.result, true
	default:
		return backend.ExecResult{}, false
	}
}

// Output returns the output captured so far.
func (p *process) Output() string { return p.out.String() }

// Stop kills the process group; it is a no-op once the process is done or
// its group is gone.
func (p *process) Stop() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := p.stop(); err != nil {
		return fmt.Errorf("shell: stop: %w", err)
	}
	return nil
}

// stop sends SIGTERM to the group now and SIGKILL after DefaultKillGrace if
// the command is still running then. Only the first call signals, and
// neither signal is sent once the group is known to be gone.
func (p *process) stop() error {
	p.stopOnce.Do(func() {
		if p.groupGone.Load() {
			return
		}
		p.stopErr = procgroup.Terminate(p.cmd.Process)
		time.AfterFunc(DefaultKillGrace, func() {
			select {
			case <-p.done:
			default:
				if !p.groupGone.Load() {
					_ = procgroup.Kill(p.cmd.Process)
				}
			}
		})
	})
	return p.stopErr
}

// copyOutput feeds the pipe into the capture until every writer is gone.
func (p *process) copyOutput() {
	defer close(p.copied)
	_, _ = io.Copy(p.out, p.outRead) // capture never fails; EOF or close ends the copy
	_ = p.outRead.Close()
}

// wait waits for the shell, removes what it left behind, and publishes the
// result.
func (p *process) wait(timer *time.Timer) {
	_ = p.cmd.Wait() // the exit status is read from ProcessState
	timer.Stop()
	p.reapGroup()
	p.drainOutput()
	p.result = p.buildResult()
	close(p.done)
}

// reapGroup kills whatever the shell left running in its process group (see
// procgroup.Reap). Once the group is gone it sets groupGone, so a late stop
// cannot signal a reused group id.
func (p *process) reapGroup() {
	if procgroup.Reap(p.cmd.Process, DefaultKillGrace) {
		p.groupGone.Store(true)
	}
}

// drainOutput waits for the copier to read the pipe to EOF. A process that
// escaped the group (setsid) could hold the pipe open forever, so after
// DefaultKillGrace the read end is closed to end the copy.
func (p *process) drainOutput() {
	select {
	case <-p.copied:
	case <-time.After(DefaultKillGrace):
		_ = p.outRead.Close()
		<-p.copied
	}
}

func (p *process) buildResult() backend.ExecResult {
	res := backend.ExecResult{
		Output:    p.out.String(),
		Truncated: p.out.truncated(),
		ExitCode:  -1,
	}
	if state := p.cmd.ProcessState; state != nil {
		res.ExitCode = state.ExitCode() // -1 when killed by a signal
	}
	if p.timedOut.Load() {
		res.TimedOut = true
		res.ExitCode = -1
	}
	if p.memKilled.Load() {
		res.Output += fmt.Sprintf(memoryNotice, p.memLimit)
	}
	return res
}
