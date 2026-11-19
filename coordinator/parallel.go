package coordinator

import (
	"context"
	"fmt"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
)

// Job is one run for RunParallel: Agent runs Input. Several jobs may share
// an agent; give each its own ThreadID (or none) so their threads stay apart.
type Job struct {
	// Agent runs the job; nil fails the job.
	Agent *agent.Agent
	// Input is the run's input, as for agent.Agent.Run.
	Input agent.Input
}

// Result is how a job ended.
type Result struct {
	// Final is the run's final event; nil when the job never ran (nil
	// Agent, or ctx ended before it started) or its stream ended without
	// one.
	Final *agent.Final
	// Err is Final.Err, or an error for a nil Agent, a missing Final or a
	// job not started because ctx ended (then ctx.Err()); nil for a run
	// that succeeded.
	Err error
}

// ParallelOptions configures RunParallel.
type ParallelOptions struct {
	// MaxConcurrency is how many jobs run at once; 0 or less means
	// DefaultMaxConcurrency.
	MaxConcurrency int
	// OnEvent, when set, sees every event of every job, with the job's index
	// in jobs. Calls are serialized, never concurrent, and a job's events
	// arrive in order; a slow OnEvent slows every job down. nil sees
	// nothing.
	OnEvent func(index int, ev agent.Event)
}

// RunParallel runs every job, at most MaxConcurrency at once, and returns one
// Result per job in input order. Cancelling ctx stops the running jobs and
// fails the ones not started with ctx.Err(). It returns once every run's
// stream has ended.
func RunParallel(ctx context.Context, jobs []Job, opts ParallelOptions) []Result {
	results := make([]Result, len(jobs))
	limit := opts.MaxConcurrency
	if limit <= 0 {
		limit = DefaultMaxConcurrency
	}
	emit := serialize(opts.OnEvent)
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, job := range jobs {
		if job.Agent == nil {
			results[i].Err = fmt.Errorf("coordinator: job %d has no agent", i)
			continue
		}
		if !acquire(ctx, slots) {
			results[i].Err = ctx.Err()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			results[i] = runJob(ctx, i, job, emit)
		}()
	}
	wg.Wait()
	return results
}

// acquire takes a slot, or reports false once ctx has ended. A ctx that ended
// wins over a free slot, so no job starts after cancellation.
func acquire(ctx context.Context, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	if ctx.Err() != nil {
		<-slots
		return false
	}
	return true
}

// runJob runs job to the end of its stream, passing each event to emit.
func runJob(ctx context.Context, index int, job Job, emit func(int, agent.Event)) Result {
	var final *agent.Final
	for ev := range job.Agent.Run(ctx, job.Input) {
		emit(index, ev)
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		return Result{Err: fmt.Errorf("coordinator: job %d ended without a final event", index)}
	}
	return Result{Final: final, Err: final.Err}
}

// serialize returns a function that calls onEvent under a lock, or does
// nothing when onEvent is nil.
func serialize(onEvent func(int, agent.Event)) func(int, agent.Event) {
	if onEvent == nil {
		return func(int, agent.Event) {}
	}
	var mu sync.Mutex
	return func(index int, ev agent.Event) {
		mu.Lock()
		defer mu.Unlock()
		onEvent(index, ev)
	}
}
