package jobs

import (
	"context"
	"errors"
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
}

func newFake() *fake { return &fake{done: make(chan struct{})} }

func (f *fake) Done() <-chan struct{} { return f.done }
func (f *fake) end()                  { f.once.Do(func() { close(f.done) }) }
func (f *fake) Stop() error {
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
	f.end()
	ch, _ := m.Changed("t")
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
	m.StopAndForget("t", []string{j.ID, "missing"}, time.Second, func() {
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
	m.Shutdown(time.Second)
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
	m.Shutdown(wait)
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
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
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

func TestRequestStopAndStop(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	j, _ := startFake(t, m, "t")
	if err := Stop(context.Background(), j, time.Second); err != nil || !j.Stopped() {
		t.Fatalf("Stop = %v, stopped %v", err, j.Stopped())
	}
	if running, err := RequestStop(j); running || err != nil {
		t.Fatalf("RequestStop on ended = %v, %v", running, err)
	}

	h, hf := startFake(t, m, "t2")
	hf.hang = true
	if err := Stop(context.Background(), h, 50*time.Millisecond); err == nil {
		t.Fatal("Stop of a hung handle returned nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Stop(ctx, h, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop with canceled ctx = %v", err)
	}
	if err := Stop[*fake](context.Background(), nil, time.Second); err == nil {
		t.Fatal("Stop accepted a nil job")
	}
	hf.end()
}

func TestShutdownWaitsForWatchers(t *testing.T) {
	m := NewManager[*fake]("x_", Limits{MaxRunning: 5, MaxFinished: 5})
	for range 3 {
		startFake(t, m, "t")
	}
	m.Shutdown(time.Second)
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
	m.Shutdown(20 * time.Millisecond)
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i == workers/2 {
				m.Shutdown(time.Second)
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
		}()
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
	go func() { m.Shutdown(2 * time.Second); close(shut) }()
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
	m.Shutdown(wait)
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
	m.Shutdown(time.Second) // returns at once: no start is left in flight
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
	m.Shutdown(20 * time.Millisecond)
	o := <-out
	if o.err != nil || !o.j.Stopped() {
		t.Fatalf("late start = %+v", o)
	}
	waitFor(t, f.done)
	exited := make(chan struct{})
	go func() { m.watchers.Wait(); close(exited) }()
	waitFor(t, exited)
}
