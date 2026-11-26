package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	cpmemory "github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// noLease hides the Leaser of the checkpointer it wraps, so runs rely on
// versions alone.
type noLease struct{ checkpoint.Checkpointer }

// countingEcho is echoTool that counts its calls.
func countingEcho(n *atomic.Int32) tool.Tool {
	return tool.New("echo", "Echo the text back.",
		func(_ context.Context, in echoInput, _ *tool.Call) (tool.Result, error) {
			n.Add(1)
			return tool.Text(in.Text), nil
		}, tool.WithReadOnly())
}

func TestStaleThreadHandleConflictsBeforeAnyTool(t *testing.T) {
	ctx := context.Background()
	cp := noLease{cpmemory.New()}
	var calls atomic.Int32
	provider := llmtest.New(
		llmtest.Turn{Text: "hello"},
		llmtest.Turn{Text: "first wins"},
		toolTurn("e1"), // never requested: the stale run ends first
	)
	a := mustNew(t, Config{Provider: provider, Tools: []tool.Tool{countingEcho(&calls)}, Checkpointer: cp})
	collect(t, mustThread(t, a, "t1").Prompt(ctx, "hi"))

	h1, h2 := mustThread(t, a, "t1"), mustThread(t, a, "t1")
	if h1.Version() == "" || h1.Version() != h2.Version() {
		t.Fatalf("handles opened at %q and %q, want the same saved version", h1.Version(), h2.Version())
	}
	if _, final := collect(t, h1.Prompt(ctx, "one")); final.Reason != ReasonCompleted {
		t.Fatalf("first handle: %+v", final)
	}
	before := len(h2.Messages())
	staleV := h2.Version()

	_, final := collect(t, h2.Prompt(ctx, "two"))
	if final.Reason != ReasonCheckpointError || !errors.Is(final.Err, checkpoint.ErrConflict) {
		t.Fatalf("stale handle: final = %+v, want checkpoint_error wrapping ErrConflict", final)
	}
	if final.Turns != 0 || provider.Calls() != 2 || calls.Load() != 0 {
		t.Fatalf("stale run went on: %d turns, %d model calls, %d tool calls", final.Turns, provider.Calls(), calls.Load())
	}
	if final.Version != staleV {
		t.Fatalf("Final.Version = %q, want the input version %q", final.Version, staleV)
	}
	// The handle keeps what it had; the store keeps the winner's history.
	if n := len(h2.Messages()); n != before || h2.Version() != staleV {
		t.Fatalf("stale handle adopted the failed run: %d messages (want %d), version %q", n, before, h2.Version())
	}
	saved, v, err := cp.Load(ctx, "t1")
	if err != nil || v != h1.Version() || len(saved.Messages()) != len(h1.Messages()) {
		t.Fatalf("store = %d messages at %q, %v; want h1's %d at %q", len(saved.Messages()), v, err, len(h1.Messages()), h1.Version())
	}
}

func TestRunVersionChain(t *testing.T) {
	ctx := context.Background()
	cp := cpmemory.New()
	a := mustNew(t, Config{
		Provider:     llmtest.New(llmtest.Turn{Text: "a"}, llmtest.Turn{Text: "b"}),
		Checkpointer: cp,
	})
	in := threadInput("one")
	_, first := collect(t, a.Run(ctx, in))
	if first.Reason != ReasonCompleted || first.Version == "" {
		t.Fatalf("first run: %+v", first)
	}
	if _, v, _ := cp.Load(ctx, "t1"); v != first.Version {
		t.Fatalf("stored version %q, Final.Version %q", v, first.Version)
	}

	// A second fresh run of the same thread is a create that conflicts.
	_, again := collect(t, a.Run(ctx, threadInput("again")))
	if !errors.Is(again.Err, checkpoint.ErrConflict) {
		t.Fatalf("fresh run onto an existing thread: %+v, want ErrConflict", again)
	}

	next := threadInput("two")
	next.State, next.Version = first.State, first.Version
	_, second := collect(t, a.Run(ctx, next))
	if second.Reason != ReasonCompleted || second.Version == first.Version {
		t.Fatalf("continued run: %+v", second)
	}
}

func TestEphemeralAndUnsavedRunsKeepInputVersion(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "a"}), Checkpointer: cpmemory.New()})
	in := threadInput("x")
	in.Ephemeral, in.Version = true, "keep:1"
	if _, final := collect(t, a.Run(context.Background(), in)); final.Version != "keep:1" {
		t.Fatalf("Final.Version = %q, want the input version", final.Version)
	}
}

// blockingTool blocks until release is closed, signalling started first.
func blockingTool(started chan<- struct{}, release <-chan struct{}) tool.Tool {
	return tool.New("echo", "Echo the text back.",
		func(ctx context.Context, in echoInput, _ *tool.Call) (tool.Result, error) {
			started <- struct{}{}
			<-release
			return tool.Text(in.Text), nil
		}, tool.WithReadOnly())
}

func TestLeaseKeepsRunsOfAThreadApart(t *testing.T) {
	ctx := context.Background()
	cp := cpmemory.New()
	started, release := make(chan struct{}, 1), make(chan struct{})
	provider := llmtest.New(toolTurn("e1"), llmtest.Turn{Text: "done"}, llmtest.Turn{Text: "later"})
	a := mustNew(t, Config{Provider: provider, Tools: []tool.Tool{blockingTool(started, release)}, Checkpointer: cp})
	h1, h2 := mustThread(t, a, "t1"), mustThread(t, a, "t1")

	var wg sync.WaitGroup
	var first *Final
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, first = collect(t, h1.Prompt(ctx, "one"))
	}()
	<-started

	events, busy := collect(t, h2.Prompt(ctx, "two"))
	if busy.Reason != ReasonBusy || !errors.Is(busy.Err, checkpoint.ErrLeaseHeld) || len(events) != 1 {
		t.Fatalf("second run while the lease is held: %d events, final = %+v", len(events), busy)
	}
	if len(h2.Messages()) != 0 {
		t.Fatal("busy run changed its handle")
	}
	close(release)
	wg.Wait()
	if first.Reason != ReasonCompleted {
		t.Fatalf("lease holder: %+v", first)
	}
	// The lease is released at the end, so a reopened handle runs.
	h3 := mustThread(t, a, "t1")
	if _, final := collect(t, h3.Prompt(ctx, "three")); final.Reason != ReasonCompleted {
		t.Fatalf("run after release: %+v", final)
	}
}

// stealableLeaser grants leases from an in-memory table that the test can
// hand to another owner.
type stealableLeaser struct {
	checkpoint.Checkpointer
	mu     sync.Mutex
	stolen bool
	renews atomic.Int32
}

func (l *stealableLeaser) Acquire(ctx context.Context, threadID, owner string, ttl time.Duration) (*checkpoint.Lease, error) {
	renew := func(context.Context) error {
		l.renews.Add(1)
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.stolen {
			return checkpoint.ErrLeaseHeld
		}
		return nil
	}
	return checkpoint.NewLease(threadID, owner, renew, func(context.Context) error { return nil }), nil
}

func (l *stealableLeaser) steal() {
	l.mu.Lock()
	l.stolen = true
	l.mu.Unlock()
}

func TestLostLeaseFailsTheNextSave(t *testing.T) {
	ctx := context.Background()
	cp := &stealableLeaser{Checkpointer: cpmemory.New()}
	started, release := make(chan struct{}, 1), make(chan struct{})
	a := mustNew(t, Config{
		Provider:     llmtest.New(toolTurn("e1"), llmtest.Turn{Text: "done"}),
		Tools:        []tool.Tool{blockingTool(started, release)},
		Checkpointer: cp,
		LeaseTTL:     9 * time.Millisecond,
	})
	done := make(chan *Final)
	go func() {
		_, final := collect(t, a.Run(ctx, threadInput("go")))
		done <- final
	}()
	<-started
	cp.steal()
	deadline := time.Now().Add(5 * time.Second)
	for cp.renews.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	final := <-done
	if final.Reason != ReasonCheckpointError || !errors.Is(final.Err, checkpoint.ErrLeaseHeld) {
		t.Fatalf("final = %+v, want checkpoint_error wrapping ErrLeaseHeld", final)
	}
}

func TestForkIsCreateOnly(t *testing.T) {
	ctx := context.Background()
	cp := cpmemory.New()
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "done"}), Checkpointer: cp})
	t1 := mustThread(t, a, "t1")
	collect(t, t1.Prompt(ctx, "hi"))
	t2, err := t1.Fork(ctx, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if _, v, _ := cp.Load(ctx, "t2"); v == "" || v != t2.Version() {
		t.Fatalf("fork version %q, stored %q", t2.Version(), v)
	}
	if _, err := cp.Save(ctx, "t3", state.New(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := t1.Fork(ctx, "t3"); !errors.Is(err, ErrThreadExists) {
		t.Fatalf("Fork onto an existing thread: %v", err)
	}
}

// hangingLeaser is a memory checkpointer whose chosen lease calls block
// until their context is done, like a call on a black-holed connection.
type hangingLeaser struct {
	checkpoint.Checkpointer
	hangAcquire, hangRenew, hangRelease bool
	renews                              atomic.Int32
	releaseErr                          chan error
}

func (l *hangingLeaser) Acquire(ctx context.Context, threadID, owner string, ttl time.Duration) (*checkpoint.Lease, error) {
	if l.hangAcquire {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	renew := func(ctx context.Context) error {
		l.renews.Add(1)
		if l.hangRenew {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	release := func(ctx context.Context) error {
		var err error
		if l.hangRelease {
			<-ctx.Done()
			err = ctx.Err()
		}
		l.releaseErr <- err
		return err
	}
	return checkpoint.NewLease(threadID, owner, renew, release), nil
}

// shortLeaseCalls bounds lease calls at d for the rest of the test.
func shortLeaseCalls(t *testing.T, d time.Duration) {
	prev := leaseCallTimeout
	leaseCallTimeout = func(time.Duration) time.Duration { return d }
	t.Cleanup(func() { leaseCallTimeout = prev })
}

func TestLeaseCallTimeout(t *testing.T) {
	for _, tc := range []struct{ ttl, want time.Duration }{
		{time.Second, 5 * time.Second},
		{10 * time.Second, 5 * time.Second},
		{30 * time.Second, 15 * time.Second},
	} {
		if got := leaseCallTimeout(tc.ttl); got != tc.want {
			t.Errorf("leaseCallTimeout(%v) = %v, want %v", tc.ttl, got, tc.want)
		}
	}
}

func TestLeaseAcquireHangBounded(t *testing.T) {
	shortLeaseCalls(t, 50*time.Millisecond)
	cp := &hangingLeaser{Checkpointer: cpmemory.New(), hangAcquire: true}
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "done"}), Checkpointer: cp, LeaseTTL: time.Second})
	done := make(chan *Final, 1)
	go func() {
		_, final := collect(t, a.Run(context.Background(), threadInput("go")))
		done <- final
	}()
	select {
	case final := <-done:
		if final.Reason != ReasonCheckpointError || !errors.Is(final.Err, context.DeadlineExceeded) {
			t.Fatalf("final = %+v, want checkpoint_error wrapping DeadlineExceeded", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run hung on a blocked lease Acquire")
	}
}

func TestLeaseRenewAndReleaseHangBounded(t *testing.T) {
	shortLeaseCalls(t, 30*time.Millisecond)
	cp := &hangingLeaser{Checkpointer: cpmemory.New(), hangRenew: true, hangRelease: true, releaseErr: make(chan error, 1)}
	started, release := make(chan struct{}, 1), make(chan struct{})
	a := mustNew(t, Config{
		Provider:     llmtest.New(toolTurn("e1"), llmtest.Turn{Text: "done"}),
		Tools:        []tool.Tool{blockingTool(started, release)},
		Checkpointer: cp,
		LeaseTTL:     9 * time.Millisecond,
	})
	done := make(chan *Final, 1)
	go func() {
		_, final := collect(t, a.Run(context.Background(), threadInput("go")))
		done <- final
	}()
	<-started
	deadline := time.Now().Add(5 * time.Second)
	for cp.renews.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cp.renews.Load() < 2 {
		t.Fatal("a hung renew blocked the renewals after it")
	}
	close(release)
	select {
	case final := <-done:
		if final.Reason != ReasonCompleted {
			t.Fatalf("final = %+v, want completed", final)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run hung on a blocked lease renew or release")
	}
	if err := <-cp.releaseErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release ended with %v, want DeadlineExceeded", err)
	}
}

// nilLeaser returns a nil lease without an error.
type nilLeaser struct{ checkpoint.Checkpointer }

func (nilLeaser) Acquire(context.Context, string, string, time.Duration) (*checkpoint.Lease, error) {
	return nil, nil
}

func TestNilLeaseIsAnError(t *testing.T) {
	a := mustNew(t, Config{
		Provider:     llmtest.New(llmtest.Turn{Text: "done"}),
		Checkpointer: nilLeaser{cpmemory.New()},
	})
	_, final := collect(t, a.Run(context.Background(), threadInput("hi")))
	if final.Reason != ReasonCheckpointError || final.Err == nil || !strings.Contains(final.Err.Error(), "Leaser returned a nil lease") {
		t.Fatalf("final = %v, %v; want a checkpoint error naming the nil lease", final.Reason, final.Err)
	}
}
