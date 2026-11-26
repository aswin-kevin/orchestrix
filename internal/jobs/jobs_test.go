package jobs

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fake is a Handle: Stop closes done unless hang is set.
type fake struct {
	done chan struct{}
	hang bool
	once sync.Once
	// onStop, when set, runs on the first Stop before the handle ends.
	onStop func()
	stop   sync.Once
	// stopErr records the Err of the ctx the first Stop got.
	stopErr  atomic.Pointer[error]
	stopOnce sync.Once
}

func newFake() *fake { return &fake{done: make(chan struct{})} }

func (f *fake) Done() <-chan struct{} { return f.done }
func (f *fake) end()                  { f.once.Do(func() { close(f.done) }) }
func (f *fake) Stop(ctx context.Context) error {
	f.stopOnce.Do(func() { err := ctx.Err(); f.stopErr.Store(&err) })
	if f.onStop != nil {
		f.stop.Do(f.onStop)
	}
	if !f.hang {
		f.end()
	}
	return nil
}

func startFake(t *testing.T, m *Manager[*fake], thread string, recorded ...string) (*Job[*fake], *fake) {
	t.Helper()
	f := newFake()
	j, err := m.Start(thread, recorded, func() (*fake, error) { return f, nil })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return j, f
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartNumbersAfterRecorded(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	j, f := startFake(t, m, "t", "x_7", "junk")
	if j.ID != "x_8" || j.ThreadID != "t" || j.Started.IsZero() {
		t.Fatalf("job = %+v", j)
	}
	f.end()
	m.MarkNotified("t", j.ID)
	eventually(t, func() bool { m.Release("t"); return m.ThreadCount() == 0 })
	j2, _ := startFake(t, m, "t", "x_8")
	if j2.ID != "x_9" {
		t.Fatalf("after release id = %s", j2.ID)
	}
}

func TestStartErrorLeavesNoJob(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 1, MaxFinished: 1})
	boom := errors.New("boom")
	if _, err := m.Start("t", nil, func() (*fake, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if j, _ := startFake(t, m, "t"); j.ID != "x_1" {
		t.Fatalf("id = %s", j.ID)
	}
}

func TestRunningLimit(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 1, MaxFinished: 3})
	_, f := startFake(t, m, "t")
	_, err := m.Start("t", nil, func() (*fake, error) { return newFake(), nil })
	var le *LimitError
	if !errors.As(err, &le) || le.Limit != 1 {
		t.Fatalf("err = %v", err)
	}
	if _, err := m.Start("other", nil, func() (*fake, error) { return newFake(), nil }); err != nil {
		t.Fatalf("other thread: %v", err)
	}
	f.end()
	if _, err := m.Start("t", nil, func() (*fake, error) { return newFake(), nil }); err != nil {
		t.Fatalf("after end: %v", err)
	}
}

// endAndWait ends f and waits until the Manager processed j's end.
func endAndWait(t *testing.T, m *Manager[*fake], j *Job[*fake], f *fake) {
	t.Helper()
	ch, _ := m.Changed(j.ThreadID)
	f.end()
	waitFor(t, ch)
}

func TestEvictionPrefersNotified(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 2})
	var ids []string
	for range 2 {
		j, f := startFake(t, m, "t")
		endAndWait(t, m, j, f)
		ids = append(ids, j.ID)
	}
	m.MarkNotified("t", ids[1])
	j3, f3 := startFake(t, m, "t")
	endAndWait(t, m, j3, f3)
	if m.Get("t", ids[1]) != nil {
		t.Fatal("the announced job was kept")
	}
	if m.Get("t", ids[0]) == nil || m.Get("t", j3.ID) == nil {
		t.Fatal("an unannounced job was evicted before the announced one")
	}
	// Without announced jobs the oldest goes.
	j4, f4 := startFake(t, m, "t")
	endAndWait(t, m, j4, f4)
	if m.Get("t", ids[0]) != nil {
		t.Fatal("the oldest job was kept")
	}
	if m.Get("t", j3.ID) == nil || m.Get("t", j4.ID) == nil {
		t.Fatal("wrong job evicted")
	}
}

func TestChangedFiresOnEnd(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	if ch, last := m.Changed("none"); ch != nil || last() != "" {
		t.Fatal("unknown thread should give a nil channel")
	}
	j, f := startFake(t, m, "t")
	ch, last := m.Changed("t")
	select {
	case <-ch:
		t.Fatal("fired early")
	default:
	}
	f.end()
	waitFor(t, ch)
	if last() != j.ID {
		t.Fatalf("last = %q", last())
	}
}

func TestReleaseOnlyWhenSettled(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	j, f := startFake(t, m, "t")
	m.Release("t")
	if m.ThreadCount() != 1 {
		t.Fatal("released a running thread")
	}
	// Take the channel before the job ends: it fires on the next end only.
	ch, _ := m.Changed("t")
	f.end()
	waitFor(t, ch)
	m.Release("t")
	if m.ThreadCount() != 1 {
		t.Fatal("released a thread with an unannounced job")
	}
	m.MarkNotified("t", j.ID, "missing")
	m.Release("t")
	if m.ThreadCount() != 0 {
		t.Fatal("settled thread kept")
	}
}

func TestStopAndForgetRefreshBeforeDrop(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	j, _ := startFake(t, m, "t")
	var sawEnded, sawKnown bool
	m.StopAndForget(t.Context(), "t", []string{j.ID, "missing"}, time.Second, func() {
		got := m.Get("t", j.ID)
		sawKnown, sawEnded = got != nil, got != nil && got.Ended()
	})
	if !sawKnown || !sawEnded {
		t.Fatalf("refresh saw known=%v ended=%v", sawKnown, sawEnded)
	}
	if m.Get("t", j.ID) != nil || m.ThreadCount() != 0 {
		t.Fatal("job or thread kept")
	}
}

func TestShutdownStopsAllAndRefuses(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	a, _ := startFake(t, m, "t1")
	b, _ := startFake(t, m, "t2")
	_ = m.Close(t.Context(), time.Second)
	for _, j := range []*Job[*fake]{m.Get("t1", a.ID), m.Get("t2", b.ID)} {
		if j == nil || !j.Ended() {
			t.Fatalf("job not stopped and kept: %+v", j)
		}
		if !j.Stopped() {
			t.Fatal("job not marked stopped")
		}
	}
	if _, err := m.Start("t1", nil, func() (*fake, error) { return newFake(), nil }); !errors.Is(err, ErrShutdown) {
		t.Fatalf("err = %v", err)
	}
}

func TestShutdownBoundedWithHungHandle(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	f := newFake()
	f.hang = true
	if _, err := m.Start("t", nil, func() (*fake, error) { return f, nil }); err != nil {
		t.Fatal(err)
	}
	const wait = 100 * time.Millisecond
	begin := time.Now()
	_ = m.Close(t.Context(), wait)
	if d := time.Since(begin); d > wait+100*time.Millisecond {
		t.Fatalf("Shutdown took %s", d)
	}
	f.end() // let the watcher goroutine go
}

func TestCloseWaitBoundedByContext(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	f := newFake()
	f.hang = true
	if _, err := m.Start("t", nil, func() (*fake, error) { return f, nil }); err != nil {
		t.Fatal(err)
	}
	defer f.end() // let the watcher goroutine go
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err := m.Close(ctx, time.Minute)
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("Close took %s, want it bounded by the context's deadline", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want the context's error", err)
	}
	if _, err := m.Start("t", nil, func() (*fake, error) { return newFake(), nil }); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Start after Close: err = %v, want ErrShutdown", err)
	}
}

func TestCloseDoneContextWithEndedJobs(t *testing.T) {
	for range 50 {
		m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
		_, f := startFake(t, m, "t")
		f.end()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := m.Close(ctx, time.Minute); err != nil {
			t.Fatalf("Close with every job ended = %v, want nil", err)
		}
		empty := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
		if err := empty.Close(ctx, time.Minute); err != nil {
			t.Fatalf("Close without jobs = %v, want nil", err)
		}
	}
}

func TestRequestStopAndStop(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	j, _ := startFake(t, m, "t")
	if err := Stop(t.Context(), j, time.Second); err != nil || !j.Stopped() {
		t.Fatalf("Stop = %v, stopped %v", err, j.Stopped())
	}
	if running, err := requestStop(t.Context(), j); running || err != nil {
		t.Fatalf("RequestStop on ended = %v, %v", running, err)
	}

	h, hf := startFake(t, m, "t2")
	hf.hang = true
	if err := Stop(t.Context(), h, 50*time.Millisecond); err == nil {
		t.Fatal("Stop of a hung handle returned nil")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Stop(ctx, h, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop with canceled ctx = %v", err)
	}
	if err := Stop[*fake](t.Context(), nil, time.Second); err == nil {
		t.Fatal("Stop accepted a nil job")
	}
	hf.end()
}

func TestShutdownWaitsForWatchers(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	for range 3 {
		startFake(t, m, "t")
	}
	_ = m.Close(t.Context(), time.Second)
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := len(m.threads["t"].finished); got != 3 {
		t.Fatalf("watchers recorded %d of 3 ends when Shutdown returned", got)
	}
}

func TestWatcherExitsAfterHungHandleEnds(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	j, f := startFake(t, m, "t")
	f.hang = true
	_ = m.Close(t.Context(), 20*time.Millisecond)
	ch, last := m.Changed("t")
	f.end()
	waitFor(t, ch)
	if last() != j.ID {
		t.Fatalf("last = %q", last())
	}
	exited := make(chan struct{})
	go func() { m.watchers.Wait(); close(exited) }()
	waitFor(t, exited)
}

func TestConcurrentStartRespectsLimitAndShutdown(t *testing.T) {
	const limit, workers = 3, 40
	m := NewManager[*fake]("x_", Limits{MaxRunning: limit, MaxFinished: 5})
	var ok, limited, refused atomic.Int32
	var mu sync.Mutex
	var started []*Job[*fake]
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			if i == workers/2 {
				_ = m.Close(t.Context(), time.Second)
				return
			}
			j, err := m.Start("t", nil, func() (*fake, error) { return newFake(), nil })
			var le *LimitError
			switch {
			case err == nil:
				ok.Add(1)
				mu.Lock()
				started = append(started, j)
				mu.Unlock()
			case errors.As(err, &le):
				limited.Add(1)
			case errors.Is(err, ErrShutdown):
				refused.Add(1)
			default:
				t.Errorf("err = %v", err)
			}
		})
	}
	wg.Wait()
	if ok.Load() > limit {
		t.Fatalf("%d starts succeeded with limit %d", ok.Load(), limit)
	}
	if ok.Load()+limited.Load()+refused.Load() != workers-1 {
		t.Fatalf("outcomes %d/%d/%d", ok.Load(), limited.Load(), refused.Load())
	}
	// A start that succeeded came before Shutdown took its snapshot, so
	// Shutdown stopped it.
	for _, j := range started {
		if !j.Stopped() || !j.Ended() {
			t.Errorf("job %s survived Shutdown: stopped %v, ended %v", j.ID, j.Stopped(), j.Ended())
		}
	}
}

func TestNewManagerRejectsBadLimits(t *testing.T) {
	for _, l := range []Limits{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewManager(%+v) did not panic", l)
				}
			}()
			NewManager[*fake]("x_", l)
		}()
	}
}

// startOutcome is what a Start returned.
type startOutcome struct {
	j   *Job[*fake]
	err error
}

// blockedStart calls Start on thread in a goroutine; its start function
// closes entered and waits for release, then returns f, or an error when f is
// nil. out yields Start's outcome.
func blockedStart(m *Manager[*fake], thread string, f *fake) (entered, release chan struct{}, out chan startOutcome) {
	entered, release, out = make(chan struct{}), make(chan struct{}), make(chan startOutcome, 1)
	go func() {
		j, err := m.Start(thread, nil, func() (*fake, error) {
			close(entered)
			<-release
			if f == nil {
				return nil, errors.New("boom")
			}
			return f, nil
		})
		out <- startOutcome{j, err}
	}()
	return entered, release, out
}

func TestSlowStartDoesNotBlockOtherThreads(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	entered, release, out := blockedStart(m, "a", newFake())
	defer func() { close(release); <-out }()
	waitFor(t, entered)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := m.Start("b", nil, func() (*fake, error) { return newFake(), nil }); err != nil {
			t.Errorf("Start on thread b: %v", err)
		}
		m.Get("a", "x_1")
		m.ThreadCount()
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("a slow start on thread a blocked the Manager")
	}
}

func TestReservedSlotCountsTowardLimit(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 1, MaxFinished: 5})
	f := newFake()
	entered, release, out := blockedStart(m, "t", f)
	waitFor(t, entered)

	var le *LimitError
	if _, err := m.Start("t", nil, func() (*fake, error) { return newFake(), nil }); !errors.As(err, &le) {
		t.Fatalf("second start during a reserved slot: err = %v", err)
	}
	m.Release("t")
	if m.ThreadCount() != 1 {
		t.Fatal("Release dropped a thread with a start in flight")
	}
	close(release)
	o := <-out
	if o.err != nil || o.j.ID != "x_1" || m.Get("t", "x_1") != o.j {
		t.Fatalf("committed start = %+v", o)
	}
	f.end()
}

func TestFailedReservationRollsBack(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 1, MaxFinished: 5})
	entered, release, out := blockedStart(m, "t", nil)
	waitFor(t, entered)
	close(release)
	if o := <-out; o.err == nil {
		t.Fatal("failed start returned no error")
	}
	// The slot is free again and the unused number is taken by the next job.
	if j, _ := startFake(t, m, "t"); j.ID != "x_1" {
		t.Fatalf("id = %s", j.ID)
	}
}

func TestShutdownDuringStart(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	f := newFake()
	entered, release, out := blockedStart(m, "t", f)
	waitFor(t, entered)

	shut := make(chan struct{})
	go func() { _ = m.Close(t.Context(), 2*time.Second); close(shut) }()
	select {
	case <-shut:
		t.Fatal("Shutdown returned while a start was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	waitFor(t, shut)
	o := <-out
	if o.err != nil {
		t.Fatalf("start committed during Shutdown: err = %v", o.err)
	}
	if !o.j.Stopped() || !o.j.Ended() {
		t.Fatalf("job started during Shutdown survived it: stopped %v, ended %v", o.j.Stopped(), o.j.Ended())
	}
	if _, err := m.Start("t", nil, func() (*fake, error) { return newFake(), nil }); !errors.Is(err, ErrShutdown) {
		t.Fatalf("err = %v", err)
	}
}

func TestShutdownWaitForStartIsBounded(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	f := newFake()
	entered, release, out := blockedStart(m, "t", f)
	waitFor(t, entered)
	const wait = 50 * time.Millisecond
	begin := time.Now()
	_ = m.Close(t.Context(), wait)
	if d := time.Since(begin); d > wait+100*time.Millisecond {
		t.Fatalf("Shutdown took %s", d)
	}
	close(release)
	o := <-out
	// The late job is stopped as it commits.
	if o.err != nil || !o.j.Stopped() || !o.j.Ended() {
		t.Fatalf("late start = %+v", o)
	}
}

func TestPanickingStartRollsBack(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 1, MaxFinished: 5})
	func() {
		defer func() { _ = recover() }()
		_, _ = m.Start("t", nil, func() (*fake, error) { panic("boom") })
	}()
	if j, _ := startFake(t, m, "t"); j.ID != "x_1" {
		t.Fatalf("id = %s", j.ID)
	}
	// returns at once: no start is left in flight
	_ = m.Close(t.Context(), time.Second)
}

// TestStartCommittingAfterShutdownTimedOut commits a start after Shutdown
// gave up waiting for it, just as Shutdown stops the other jobs and would
// wait for their watchers: the commit adds a watcher, which must not race
// a WaitGroup Wait.
func TestStartCommittingAfterShutdownTimedOut(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	f := newFake()
	entered, release, out := blockedStart(m, "b", f)
	waitFor(t, entered)
	a := newFake()
	a.onStop = func() { close(release) } // B commits as Shutdown stops A
	if _, err := m.Start("a", nil, func() (*fake, error) { return a, nil }); err != nil {
		t.Fatal(err)
	}
	_ = m.Close(t.Context(), 20*time.Millisecond)
	o := <-out
	if o.err != nil || !o.j.Stopped() {
		t.Fatalf("late start = %+v", o)
	}
	waitFor(t, f.done)
	exited := make(chan struct{})
	go func() { m.watchers.Wait(); close(exited) }()
	waitFor(t, exited)
}

// Stops the Manager sends as a run or the Manager ends must go out even when
// the caller's ctx already ended.
func TestStopRequestsOutliveCallerCtx(t *testing.T) {
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	j1, f1 := startFake(t, m, "t")
	m.StopAndForget(ended, "t", []string{j1.ID}, time.Second, func() {})
	_, f2 := startFake(t, m, "u")
	_ = m.Close(ended, time.Second)
	for i, f := range []*fake{f1, f2} {
		got := f.stopErr.Load()
		if got == nil {
			t.Fatalf("handle %d was not stopped", i+1)
		}
		if *got != nil {
			t.Errorf("handle %d: Stop got a ctx with Err %v, want a live one", i+1, *got)
		}
	}
}

// stopAll sends its stop requests concurrently: a slow request for one job
// does not hold back the request for the next.
func TestStopAllSendsConcurrently(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	j1, f1 := startFake(t, m, "t")
	j2, f2 := startFake(t, m, "t")
	entered1, entered2 := make(chan struct{}), make(chan struct{})
	var overlapped1, overlapped2 atomic.Bool
	// Each Stop waits briefly for the other to be entered: sent one after
	// the other, the first gives up before the second is sent.
	await := func(mine, other chan struct{}, overlapped *atomic.Bool) func() {
		return func() {
			close(mine)
			select {
			case <-other:
				overlapped.Store(true)
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	f1.onStop = await(entered1, entered2, &overlapped1)
	f2.onStop = await(entered2, entered1, &overlapped2)
	m.StopAndForget(t.Context(), "t", []string{j1.ID, j2.ID}, time.Second, func() {})
	if !overlapped1.Load() || !overlapped2.Load() {
		t.Fatalf("stop requests were sent one after the other (overlapped %v, %v)", overlapped1.Load(), overlapped2.Load())
	}
}

// episodic is a Handle whose Done channel is replaced for each episode, as
// a handle that Restart runs again.
type episodic struct {
	mu   sync.Mutex
	done chan struct{}
}

func newEpisodic() *episodic { return &episodic{done: make(chan struct{})} }

func (e *episodic) Done() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done
}

// end closes the current episode's channel.
func (e *episodic) end() {
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.done:
	default:
		close(e.done)
	}
}

// rearm starts a new episode.
func (e *episodic) rearm() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.done = make(chan struct{})
	return nil
}

func (e *episodic) Stop(context.Context) error { e.end(); return nil }

func startEpisodic(t *testing.T, m *Manager[*episodic], thread string) (*Job[*episodic], *episodic) {
	t.Helper()
	e := newEpisodic()
	j, err := m.Start(thread, nil, func() (*episodic, error) { return e, nil })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return j, e
}

func TestRestartReopensJob(t *testing.T) {
	m := NewManager[*episodic]("x_", Limits{MaxRunning: 2, MaxFinished: 5})
	j, e := startEpisodic(t, m, "t")
	ch, last := m.Changed("t")
	old := e.Done()
	e.end()
	waitFor(t, ch)
	m.MarkNotified("t", j.ID)
	j.stopped.Store(true) // as if it had been stopped
	j2, err := m.Restart("t", j.ID, e.rearm)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if j2 != j || m.Get("t", j.ID) != j {
		t.Fatalf("Restart returned %p, Get %p; want the same job %p", j2, m.Get("t", j.ID), j)
	}
	select {
	case <-old:
	default:
		t.Fatal("the first episode's Done reopened")
	}
	if j.Ended() || j.Notified() || j.Stopped() {
		t.Fatalf("restarted job: ended %v notified %v stopped %v; want all false", j.Ended(), j.Notified(), j.Stopped())
	}
	ch, _ = m.Changed("t")
	e.end()
	waitFor(t, ch)
	if last() != j.ID {
		t.Fatalf("last ended = %q, want %q", last(), j.ID)
	}
}

func TestRestartLimit(t *testing.T) {
	m := NewManager[*episodic]("x_", Limits{MaxRunning: 1, MaxFinished: 5})
	j, e := startEpisodic(t, m, "t")
	e.end()
	eventually(t, j.Ended)
	// The ended job does not count against the limit: MaxRunning 1 can run
	// its only job again.
	if _, err := m.Restart("t", j.ID, e.rearm); err != nil {
		t.Fatalf("Restart of the only job: %v", err)
	}
	e.end()
	eventually(t, j.Ended)
	_, other := startEpisodic(t, m, "t")
	called := false
	_, err := m.Restart("t", j.ID, func() error { called = true; return e.rearm() })
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Limit != 1 {
		t.Fatalf("Restart with the thread full = %v, want LimitError{1}", err)
	}
	if called {
		t.Fatal("start was called although no slot was free")
	}
	other.end()
	// A failed start rolls the reservation back and leaves the job ended.
	boom := errors.New("boom")
	if _, err := m.Restart("t", j.ID, func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Restart = %v, want boom", err)
	}
	if !j.Ended() || m.Get("t", j.ID) != j {
		t.Fatal("a failed restart changed the job")
	}
	if _, err := m.Restart("t", j.ID, e.rearm); err != nil {
		t.Fatalf("Restart after a rolled-back one: %v", err)
	}
}

func TestRestartRunningIsError(t *testing.T) {
	m := NewManager[*episodic]("x_", Limits{MaxRunning: 2, MaxFinished: 5})
	j, e := startEpisodic(t, m, "t")
	if _, err := m.Restart("t", j.ID, e.rearm); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("Restart of a running job = %v, want ErrUnknownJob", err)
	}
	if _, err := m.Restart("t", "x_9", e.rearm); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("Restart of an unknown job = %v, want ErrUnknownJob", err)
	}
	e.end()
	eventually(t, j.Ended)
	// A restart in flight holds the job: a second one is refused.
	entered, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = m.Restart("t", j.ID, func() error { close(entered); <-release; return e.rearm() })
	}()
	<-entered
	if _, err := m.Restart("t", j.ID, e.rearm); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("second Restart while one is in flight = %v, want ErrUnknownJob", err)
	}
	close(release)
	if err := m.Close(t.Context(), time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := m.Restart("t", j.ID, e.rearm); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Restart after Close = %v, want ErrShutdown", err)
	}
}

func TestAdoptContinuesNumbering(t *testing.T) {
	m := NewManager[*episodic]("bg_", Limits{MaxRunning: 2, MaxFinished: 5})
	e := newEpisodic()
	e.end()
	started := time.Now().Add(-time.Hour)
	j, err := m.Adopt("t", "bg_7", started, e)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if j.ID != "bg_7" || !j.Started.Equal(started) || m.Get("t", "bg_7") != j {
		t.Fatalf("adopted job = %+v", j)
	}
	if _, err := m.Adopt("t", "bg_7", started, newEpisodic()); err == nil {
		t.Fatal("Adopt of a held id succeeded")
	}
	j2, _ := startEpisodic(t, m, "t")
	if j2.ID != "bg_8" {
		t.Fatalf("next id = %q, want bg_8", j2.ID)
	}
	// A running adopted job takes a slot and is watched.
	running := newEpisodic()
	ja, err := m.Adopt("t", "bg_3", started, running)
	if err != nil {
		t.Fatalf("Adopt running: %v", err)
	}
	var limit *LimitError
	if _, err := m.Adopt("t", "bg_4", started, newEpisodic()); !errors.As(err, &limit) {
		t.Fatalf("Adopt over the limit = %v, want LimitError", err)
	}
	ch, last := m.Changed("t")
	running.end()
	waitFor(t, ch)
	if last() != ja.ID {
		t.Fatalf("last ended = %q, want %q", last(), ja.ID)
	}
}

func TestPinnedJobIsNotEvicted(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 1})
	j1, f1 := startFake(t, m, "t")
	var unpinned atomic.Bool
	m.SetPinned(func(f *fake) bool { return f == f1 && !unpinned.Load() })
	endAndWait(t, m, j1, f1)
	// The pinned job does not count against MaxFinished: the next ended
	// job is kept beside it, and the one after that evicts it, not j1.
	j2, f2 := startFake(t, m, "t")
	endAndWait(t, m, j2, f2)
	if m.Get("t", j1.ID) == nil || m.Get("t", j2.ID) == nil {
		t.Fatal("a job was evicted while only one unpinned job had ended")
	}
	j3, f3 := startFake(t, m, "t")
	endAndWait(t, m, j3, f3)
	if m.Get("t", j1.ID) == nil {
		t.Fatal("the pinned job was evicted")
	}
	if m.Get("t", j2.ID) != nil || m.Get("t", j3.ID) == nil {
		t.Fatal("wrong job evicted")
	}
	// Unpinned, it is an ordinary ended job again: the next end evicts the
	// oldest.
	unpinned.Store(true)
	j4, f4 := startFake(t, m, "t")
	endAndWait(t, m, j4, f4)
	if m.Get("t", j1.ID) != nil || m.Get("t", j3.ID) != nil || m.Get("t", j4.ID) == nil {
		t.Fatal("the unpinned jobs were not trimmed to MaxFinished")
	}
}

func TestRestartOfForgottenJobIsStopped(t *testing.T) {
	m := NewManager[*episodic]("x_", Limits{MaxRunning: 2, MaxFinished: 5})
	j, e := startEpisodic(t, m, "t")
	ch, _ := m.Changed("t")
	e.end()
	waitFor(t, ch)
	_, err := m.Restart("t", j.ID, func() error {
		// The nested run that owns the job ends while it restarts.
		m.StopAndForget(t.Context(), "t", []string{j.ID}, time.Second, func() {})
		return e.rearm()
	})
	if !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("Restart = %v, want ErrUnknownJob", err)
	}
	if m.Get("t", j.ID) != nil {
		t.Fatal("the forgotten job was registered again")
	}
	if !j.Ended() || !j.Stopped() {
		t.Fatalf("the restarted episode: ended %v stopped %v; want it stopped", j.Ended(), j.Stopped())
	}
}

// finishedCount returns how many times id is in thread's finished list.
func finishedCount[H Handle](m *Manager[H], thread, id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	if t, ok := m.threads[thread]; ok {
		for _, f := range t.finished {
			if f == id {
				n++
			}
		}
	}
	return n
}

func TestRestartBeforeWatcherRecordsEnd(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for range 20 {
		m := NewManager[*episodic]("x_", Limits{MaxRunning: 2, MaxFinished: 5})
		j, e := startEpisodic(t, m, "t")
		for range 10 {
			runtime.Gosched() // the watcher now waits on the first episode
		}
		ch, _ := m.Changed("t")
		// The episode ends and the job is restarted at once, before its
		// watcher (on the only P) records the end.
		e.end()
		if _, err := m.Restart("t", j.ID, e.rearm); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		for range 10 {
			runtime.Gosched() // let the old watcher run
		}
		time.Sleep(time.Millisecond)
		if n := finishedCount(m, "t", j.ID); n != 0 {
			t.Fatalf("the running job is %d times in the finished list", n)
		}
		select {
		case <-ch:
			t.Fatal("Changed fired for an episode Restart had already taken")
		default:
		}
		e.end()
		waitFor(t, ch)
		if n := finishedCount(m, "t", j.ID); n != 1 {
			t.Fatalf("the ended job is %d times in the finished list, want 1", n)
		}
	}
}

func TestFailedRestartBeforeWatcherRecordsEnd(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	m := NewManager[*episodic]("x_", Limits{MaxRunning: 2, MaxFinished: 5})
	j, e := startEpisodic(t, m, "t")
	for range 10 {
		runtime.Gosched()
	}
	ch, _ := m.Changed("t")
	e.end()
	if _, err := m.Restart("t", j.ID, func() error { return errors.New("no") }); err == nil {
		t.Fatal("Restart succeeded")
	}
	waitFor(t, ch)
	if n := finishedCount(m, "t", j.ID); n != 1 {
		t.Fatalf("the ended job is %d times in the finished list, want 1", n)
	}
}

func TestAdoptStartingReservesFirst(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 1, MaxFinished: 5})
	startFake(t, m, "t")
	called := false
	_, err := m.AdoptStarting("t", "x_7", time.Now(), func() (*fake, error) { called = true; return newFake(), nil })
	var limit *LimitError
	if !errors.As(err, &limit) || called {
		t.Fatalf("AdoptStarting at the limit = %v (start called %v), want a LimitError before start", err, called)
	}
	m2 := NewManager[*fake]("x_", Limits{MaxRunning: 1, MaxFinished: 5})
	if _, err := m2.AdoptStarting("t", "x_7", time.Now(), func() (*fake, error) { return nil, errors.New("no") }); err == nil {
		t.Fatal("a failed start was adopted")
	}
	f := newFake()
	j, err := m2.AdoptStarting("t", "x_7", time.Now(), func() (*fake, error) { return f, nil })
	if err != nil || m2.Get("t", "x_7") != j {
		t.Fatalf("AdoptStarting = %v, %v", j, err)
	}
	// The slot is taken and numbering continues after the adopted id.
	if _, err := m2.Start("t", nil, func() (*fake, error) { return newFake(), nil }); !errors.As(err, &limit) {
		t.Fatalf("Start beside the adopted running job = %v, want a LimitError", err)
	}
	f.end()
	eventually(t, j.Ended)
	j2, _ := startFake(t, m2, "t")
	if j2.ID != "x_8" {
		t.Fatalf("next id = %s, want x_8", j2.ID)
	}
}

func TestWaitLimit(t *testing.T) {
	const max = 5 * time.Minute
	tests := []struct {
		seconds int
		want    time.Duration
	}{
		{-1, max}, {0, max}, {1, time.Second}, {300, max}, {301, max}, {1 << 40, max},
	}
	for _, tt := range tests {
		if got := WaitLimit(tt.seconds, max); got != tt.want {
			t.Errorf("WaitLimit(%d) = %v, want %v", tt.seconds, got, tt.want)
		}
	}
}
