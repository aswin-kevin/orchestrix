package tasks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/internal/jobs"
)

// stopWait bounds how long stopping jobs waits for them to end: a job that
// ignores SIGTERM is killed after the backend's grace period, and the second
// period covers reaping what it left behind.
const stopWait = 2 * backend.DefaultKillGrace

// jobIDPrefix starts every job id; the rest is the job's number in its thread.
const jobIDPrefix = "bg_"

// errShutdown refuses jobs once the middleware has been shut down.
var errShutdown = errors.New("background tasks are shut down")

// process is the handle of one started command: the backend's process and the
// command line it runs.
type process struct {
	backend.Process
	command string
}

// job is one started command in the shared registry.
type job = jobs.Job[*process]

// manager is the registry of the middleware's jobs, shared by the concurrent
// runs of every agent using it.
type manager = jobs.Manager[*process]

func newManager(maxRunning, maxFinished int) *manager {
	return jobs.NewManager[*process](jobIDPrefix, jobs.Limits{MaxRunning: maxRunning, MaxFinished: maxFinished})
}

// startJob launches command as the next job of threadID. recorded lists the
// jobs the run's State already knows, so a thread resumed in a new process
// never reuses the id of a job it recorded before.
func startJob(ctx context.Context, mg *manager, starter backend.Starter, threadID, command string, timeout time.Duration, recorded []JobRecord) (*job, error) {
	ids := make([]string, len(recorded))
	for i, r := range recorded {
		ids[i] = r.ID
	}
	j, err := mg.Start(threadID, ids, func() (*process, error) {
		proc, err := starter.Start(ctx, command, backend.ExecOptions{Timeout: timeout})
		if err != nil {
			return nil, err
		}
		return &process{Process: proc, command: command}, nil
	})
	var limit *jobs.LimitError
	switch {
	case errors.Is(err, jobs.ErrShutdown):
		return nil, errShutdown
	case errors.As(err, &limit):
		return nil, fmt.Errorf("too many background tasks running (limit %d); stop one with %s or wait for one to finish", limit.Limit, ToolTaskStop)
	}
	return j, err
}

// outcome returns the job's status and exit code; the exit code is 0 while
// the job runs.
func outcome(j *job) (status string, exitCode int) {
	res, ok := j.Handle.Result()
	switch {
	case !ok:
		return StatusRunning, 0
	case j.Stopped() && res.ExitCode != 0:
		// A job that exited cleanly as the stop raced it completed.
		return StatusStopped, res.ExitCode
	case res.TimedOut:
		return StatusTimedOut, res.ExitCode
	case res.ExitCode == 0:
		return StatusCompleted, 0
	}
	return StatusFailed, res.ExitCode
}

// stop stops j and waits, up to stopWait or until ctx ends, for it to end.
func stop(ctx context.Context, j *job) error { return jobs.Stop(ctx, j, stopWait) }
