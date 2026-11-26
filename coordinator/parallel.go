package coordinator

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
)

// Job is one run for RunParallel: Agent runs Input. Several jobs may share
// an agent; give each its own Input.ThreadID so their threads stay apart.
// A job without one runs without a thread: with the preset's per-thread
// default backend it gets a fresh in-memory workspace of its own, dropped
// when it ends, and it cannot start background tasks (start_task needs a
// thread).
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
	// job not started because ctx ended (then ctx.Err()). It is nil when
	// Final carries no error; check Final.Reason for completion (max_turns
	// and interrupted end with a nil Err).
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
// stream has ended, so a job whose agent ignores ctx keeps it from
// returning.
//
// One failing job does not stop or cancel the others. A panic in a run's own
// code (its provider, a ModelWrapper, its Checkpointer) ends that run alone
// with agent.ReasonInternalError, like any other failure. A panic on a job's
// goroutine that the agent does not recover (in OnEvent, say) is the
// exception: it cancels every job, and once they have ended RunParallel
// panics on the caller's goroutine with the first such panic, whose message
// carries the job goroutine's stack (errors.Unwrap gives back the original
// value when it was an error).
func RunParallel(ctx context.Context, jobs []Job, opts ParallelOptions) []Result {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var crash firstPanic
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
		wg.Go(func() {
			defer func() { <-slots }()
			defer crash.capture(cancel)
			results[i] = runJob(ctx, i, job, emit)
		})
	}
	wg.Wait()
	crash.rethrow()
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

// firstPanic keeps the first panic of the job goroutines, to be raised again
// on the caller's goroutine.
type firstPanic struct {
	mu  sync.Mutex
	err *jobPanic
}

// capture, deferred on a job goroutine, recovers a panic, keeps it if it is
// the first, and cancels the other jobs.
func (f *firstPanic) capture(cancel context.CancelFunc) {
	v := recover()
	if v == nil {
		return
	}
	f.mu.Lock()
	if f.err == nil {
		f.err = &jobPanic{value: v, stack: debug.Stack()}
	}
	f.mu.Unlock()
	cancel()
}

// rethrow panics with the kept panic, if any; call it once every job ended.
func (f *firstPanic) rethrow() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		panic(f.err)
	}
}

// jobPanic is a panic recovered on a job goroutine, with that goroutine's
// stack.
type jobPanic struct {
	value any
	stack []byte
}

func (p *jobPanic) Error() string {
	return fmt.Sprintf("coordinator: panic in a RunParallel job: %v\n\n%s", p.value, p.stack)
}

// Unwrap returns the original panic value when it is an error.
func (p *jobPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}
