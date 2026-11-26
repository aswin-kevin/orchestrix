package coordinator

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// testTimeout bounds every wait in these tests, so a bug fails them instead
// of hanging them.
const testTimeout = 5 * time.Second

// echo is a provider that answers each request with "answer to " and the
// text of its last message, so an answer names the job it belongs to
// whatever order the requests arrive in. It counts the streams running at
// once. With holdFor set, a request waits until holdFor streams run at once
// (or testTimeout passes); with block set, it waits for its context to end.
type echo struct {
	holdFor int
	block   bool

	mu      sync.Mutex
	running int
	peak    int
	isFull  bool
	full    chan struct{} // closed once holdFor streams ran at once
	entered chan struct{} // receives one value per request
}

func newEcho() *echo {
	return &echo{full: make(chan struct{}), entered: make(chan struct{}, 64)}
}

// Stream implements llm.Provider.
func (e *echo) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		e.enter()
		defer e.leave()
		select {
		case e.entered <- struct{}{}:
		default:
		}
		if e.block {
			<-ctx.Done()
			yield(llm.StreamEvent{}, ctx.Err())
			return
		}
		if e.holdFor > 0 {
			select {
			case <-e.full:
			case <-time.After(testTimeout):
			case <-ctx.Done():
				yield(llm.StreamEvent{}, ctx.Err())
				return
			}
		}
		text := "answer to " + req.Messages[len(req.Messages)-1].Text()
		for ev, err := range llmtest.New(llmtest.Turn{Text: text}).Stream(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// Complete implements llm.Provider.
func (e *echo) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, e, req)
}

func (e *echo) enter() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running++
	e.peak = max(e.peak, e.running)
	if e.running == e.holdFor && !e.isFull {
		e.isFull = true
		close(e.full)
	}
}

func (e *echo) leave() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running--
}

// stats returns the streams running now and the most that ran at once.
func (e *echo) stats() (running, peak int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running, e.peak
}

func newAgent(t *testing.T, p llm.Provider) *agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Config{Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// jobsFor returns n jobs on a, the i-th asking "job i".
func jobsFor(a *agent.Agent, n int) []Job {
	jobs := make([]Job, n)
	for i := range jobs {
		jobs[i] = Job{Agent: a, Input: agent.Input{Messages: []llm.Message{llm.UserText(fmt.Sprintf("job %d", i))}}}
	}
	return jobs
}

// runBounded runs RunParallel and fails the test if it does not return
// within testTimeout.
func runBounded(t *testing.T, ctx context.Context, jobs []Job, opts ParallelOptions) []Result {
	t.Helper()
	done := make(chan []Result, 1)
	go func() { done <- RunParallel(ctx, jobs, opts) }()
	select {
	case res := <-done:
		return res
	case <-time.After(testTimeout):
		t.Fatal("RunParallel did not return")
		return nil
	}
}

func TestRunParallelBoundedConcurrency(t *testing.T) {
	p := newEcho()
	p.holdFor = 3
	res := runBounded(t, context.Background(), jobsFor(newAgent(t, p), 8), ParallelOptions{MaxConcurrency: 3})
	if _, peak := p.stats(); peak != 3 {
		t.Errorf("peak concurrent model calls = %d, want 3", peak)
	}
	if len(res) != 8 {
		t.Fatalf("got %d results, want 8", len(res))
	}
	for i, r := range res {
		if r.Err != nil || r.Final == nil || r.Final.Reason != agent.ReasonCompleted {
			t.Errorf("result %d = %+v, want a completed run", i, r)
			continue
		}
		if want := fmt.Sprintf("answer to job %d", i); r.Final.Text != want {
			t.Errorf("result %d text = %q, want %q", i, r.Final.Text, want)
		}
	}
}

func TestRunParallelDefaultConcurrency(t *testing.T) {
	p := newEcho()
	p.holdFor = DefaultMaxConcurrency
	runBounded(t, context.Background(), jobsFor(newAgent(t, p), 2*DefaultMaxConcurrency), ParallelOptions{})
	if _, peak := p.stats(); peak != DefaultMaxConcurrency {
		t.Errorf("peak concurrent model calls = %d, want %d", peak, DefaultMaxConcurrency)
	}
}

func TestRunParallelCancel(t *testing.T) {
	p := newEcho()
	p.block = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-p.entered:
			cancel()
		case <-time.After(testTimeout):
		}
	}()
	res := runBounded(t, ctx, jobsFor(newAgent(t, p), 4), ParallelOptions{MaxConcurrency: 1})
	if running, _ := p.stats(); running != 0 {
		t.Errorf("%d model calls still running after RunParallel returned", running)
	}
	if f := res[0].Final; f == nil || f.Reason != agent.ReasonAbortedStreaming || res[0].Err == nil {
		t.Errorf("started job = %+v, want an aborted run with its error", res[0])
	}
	for i, r := range res[1:] {
		if !errors.Is(r.Err, context.Canceled) || r.Final != nil {
			t.Errorf("unstarted job %d = %+v, want context.Canceled and no Final", i+1, r)
		}
	}
	if _, peak := p.stats(); peak != 1 {
		t.Errorf("peak = %d, want only the first job started", peak)
	}
}

func TestRunParallelCancelledBeforeStart(t *testing.T) {
	p := newEcho()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := runBounded(t, ctx, jobsFor(newAgent(t, p), 3), ParallelOptions{})
	for i, r := range res {
		if !errors.Is(r.Err, context.Canceled) {
			t.Errorf("job %d err = %v, want context.Canceled", i, r.Err)
		}
	}
	if _, peak := p.stats(); peak != 0 {
		t.Errorf("%d jobs started on a cancelled context", peak)
	}
}

func TestRunParallelOnEventSerialized(t *testing.T) {
	const n = 8
	a := newAgent(t, newEcho())
	var (
		// deliberately unsynchronized: -race flags overlapping calls
		counter  int
		received atomic.Int64
		inside   atomic.Bool
		overlap  atomic.Bool
		finals   = make([]int, n)
		lastKind = make([]agent.EventKind, n)
	)
	onEvent := func(i int, ev agent.Event) {
		if !inside.CompareAndSwap(false, true) {
			overlap.Store(true)
		}
		defer inside.Store(false)
		received.Add(1)
		counter++
		lastKind[i] = ev.Kind
		if ev.Kind == agent.KindFinal {
			finals[i]++
		}
		time.Sleep(time.Millisecond) // widen the window for an overlap
	}
	res := runBounded(t, context.Background(), jobsFor(a, n), ParallelOptions{MaxConcurrency: n, OnEvent: onEvent})
	if overlap.Load() {
		t.Error("OnEvent calls overlapped")
	}
	if got := received.Load(); int64(counter) != got || got == 0 {
		t.Errorf("counter = %d, events received = %d; want equal and non-zero", counter, got)
	}
	for i := range n {
		if finals[i] != 1 || lastKind[i] != agent.KindFinal {
			t.Errorf("job %d: %d final events, last kind %q; want one final, last", i, finals[i], lastKind[i])
		}
		if res[i].Err != nil {
			t.Errorf("job %d err = %v", i, res[i].Err)
		}
	}
}

func TestRunParallelNilAgent(t *testing.T) {
	jobs := jobsFor(newAgent(t, newEcho()), 2)
	jobs[0].Agent = nil
	res := runBounded(t, context.Background(), jobs, ParallelOptions{})
	if res[0].Err == nil || res[0].Final != nil {
		t.Errorf("nil agent result = %+v, want an error and no Final", res[0])
	}
	if res[1].Err != nil || res[1].Final == nil || res[1].Final.Text != "answer to job 1" {
		t.Errorf("valid job result = %+v, want its answer", res[1])
	}
}

func TestRunParallelNoJobs(t *testing.T) {
	if res := RunParallel(context.Background(), nil, ParallelOptions{}); len(res) != 0 {
		t.Errorf("got %d results for no jobs", len(res))
	}
}

func TestRunParallelOnEventPanicSurfaces(t *testing.T) {
	p := newEcho()
	p.block = true // the other jobs only end when RunParallel cancels them
	jobs := jobsFor(newAgent(t, p), 3)
	jobs[0].Agent = newAgent(t, newEcho())
	before := runtime.NumGoroutine()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		RunParallel(context.Background(), jobs, ParallelOptions{OnEvent: func(i int, ev agent.Event) {
			if i == 0 {
				panic("boom in OnEvent")
			}
		}})
	}()
	if recovered == nil || !strings.Contains(fmt.Sprint(recovered), "boom in OnEvent") {
		t.Fatalf("recovered %v, want the OnEvent panic on the caller", recovered)
	}
	if !strings.Contains(fmt.Sprint(recovered), "goroutine") {
		t.Errorf("recovered %v, want the job goroutine's stack", recovered)
	}
	if running, _ := p.stats(); running != 0 {
		t.Errorf("%d model calls still running after the panic surfaced", running)
	}
	deadline := time.Now().Add(testTimeout)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Errorf("%d goroutines left running (%d before)", n-before, before)
	}
}

func TestRunParallelPanicUnwrapsError(t *testing.T) {
	sentinel := errors.New("sentinel")
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		RunParallel(context.Background(), jobsFor(newAgent(t, newEcho()), 1), ParallelOptions{OnEvent: func(int, agent.Event) { panic(sentinel) }})
	}()
	err, ok := recovered.(error)
	if !ok || !errors.Is(err, sentinel) {
		t.Errorf("recovered %v, want an error wrapping the panic value", recovered)
	}
}
