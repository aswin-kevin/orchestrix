package asynctest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/middleware/async"
)

// Scenario is the behaviour a StartFunc must give the task it starts.
type Scenario int

const (
	// ScenarioComplete ends completed with non-empty Text.
	ScenarioComplete Scenario = iota
	// ScenarioRunUntilStopped runs until Stop.
	ScenarioRunUntilStopped
	// ScenarioWait stops waiting for a decision: its Outcome is
	// async.StatusWaiting with a Pending interrupt of at least one request.
	// A Handle that is an async.Resolver must, once every request is
	// approved, run a next episode that ends completed.
	ScenarioWait
)

// String returns the scenario's name.
func (sc Scenario) String() string {
	switch sc {
	case ScenarioComplete:
		return "Complete"
	case ScenarioRunUntilStopped:
		return "RunUntilStopped"
	case ScenarioWait:
		return "Wait"
	}
	return "Scenario(?)"
}

// StartFunc starts a task for sc and returns its Handle; nil skips sc. It
// fails t itself when the task cannot be started.
type StartFunc func(t *testing.T, sc Scenario) async.Handle

// episodeTimeout bounds how long the suite waits for a ScenarioComplete or
// ScenarioWait episode to stop (see Run).
const episodeTimeout = 30 * time.Second

// stopTimeout bounds how long the suite waits for a stopped episode to end
// (see Run).
const stopTimeout = 5 * time.Second

// Run executes the conformance suite, one subtest per check, each with tasks
// of its own from start. Every Handle start returns that is still running
// when its subtest ends is stopped then. The suite waits up to 30 seconds
// for a ScenarioComplete or ScenarioWait episode (or a next episode) to
// stop, and up to 5 seconds for a Stop to end an episode; a Handle slower
// than that fails.
func Run(t *testing.T, start StartFunc) {
	t.Helper()
	tests := []struct {
		name string
		sc   Scenario
		fn   func(t *testing.T, h async.Handle)
	}{
		{"OutcomeBeforeDone", ScenarioRunUntilStopped, testOutcomeBeforeDone},
		{"DoneSameChannel", ScenarioRunUntilStopped, testDoneSameChannel},
		{"DoneStaysClosed", ScenarioComplete, testDoneStaysClosed},
		{"WaitStaysClosed", ScenarioWait, testDoneStaysClosed},
		{"CompleteOutcome", ScenarioComplete, testCompleteOutcome},
		{"StopCancels", ScenarioRunUntilStopped, testStopCancels},
		{"SendAfterDone", ScenarioComplete, testSendAfterDone},
		{"WaitOutcome", ScenarioWait, testWaitOutcome},
		{"ContinueReturnsNewHandle", ScenarioComplete, testContinueReturnsNewHandle},
		{"ContinueOnce", ScenarioComplete, testContinueOnce},
		{"ResolveReturnsNewHandle", ScenarioWait, testResolveReturnsNewHandle},
		{"ResolveOnce", ScenarioWait, testResolveOnce},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, tt.sc)
			if h == nil {
				t.Skipf("the runner does not support scenario %s", tt.sc)
			}
			t.Cleanup(func() {
				if isClosed(h.Done()) {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
				defer cancel()
				_ = h.Stop(ctx)
				select {
				case <-h.Done():
				case <-ctx.Done():
				}
			})
			tt.fn(t, h)
		})
	}
}

// stopLater stops h, when it still runs, as t ends.
func stopLater(t *testing.T, h async.Handle) {
	t.Cleanup(func() {
		if isClosed(h.Done()) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		_ = h.Stop(ctx)
		select {
		case <-h.Done():
		case <-ctx.Done():
		}
	})
}

// waitDone waits up to d for h's Done, failing t when it stays open.
func waitDone(t *testing.T, h async.Handle, d time.Duration, what string) {
	t.Helper()
	select {
	case <-h.Done():
	case <-time.After(d):
		t.Fatalf("Done not closed within %v of %s", d, what)
	}
}

// isClosed reports whether ch is closed, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// outcome returns h's Outcome, failing t when ok is false.
func outcome(t *testing.T, h async.Handle) async.Outcome {
	t.Helper()
	out, ok := h.Outcome()
	if !ok {
		t.Fatal("Outcome ok = false after Done closed")
	}
	return out
}

func testOutcomeBeforeDone(t *testing.T, h async.Handle) {
	if isClosed(h.Done()) {
		t.Fatal("Done closed before Stop in a task that runs until stopped")
	}
	if out, ok := h.Outcome(); ok {
		t.Fatalf("Outcome = %+v, true before Done closed; want ok false", out)
	}
}

func testDoneSameChannel(t *testing.T, h async.Handle) {
	if a, b := h.Done(), h.Done(); a != b {
		t.Fatal("Done returned a different channel on a second call")
	}
}

// sameOutcome reports whether a and b are the same Outcome, Pending
// compared by value.
func sameOutcome(a, b async.Outcome) bool {
	return a.Status == b.Status && a.Text == b.Text && a.Error == b.Error && a.Usage == b.Usage && reflect.DeepEqual(a.Pending, b.Pending)
}

func testDoneStaysClosed(t *testing.T, h async.Handle) {
	waitDone(t, h, episodeTimeout, "the start")
	first := outcome(t, h)
	// Stop is a no-op once Done: the episode must not reopen nor change.
	ctx, cancel := context.WithTimeout(t.Context(), stopTimeout)
	defer cancel()
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop after Done = %v, want nil", err)
	}
	for i := range 3 {
		if !isClosed(h.Done()) {
			t.Fatalf("Done reopened (read %d after it closed)", i+1)
		}
	}
	if again := outcome(t, h); !sameOutcome(first, again) {
		t.Fatalf("Outcome changed after Done: %+v, then %+v", first, again)
	}
}

func testCompleteOutcome(t *testing.T, h async.Handle) {
	waitDone(t, h, episodeTimeout, "the start")
	out := outcome(t, h)
	if out.Status != async.StatusCompleted {
		t.Fatalf("Status = %q, want %q (outcome %+v)", out.Status, async.StatusCompleted, out)
	}
	if out.Text == "" {
		t.Error("Text is empty, want the task's answer")
	}
	if out.Pending != nil {
		t.Errorf("Pending = %+v on a completed task, want nil", out.Pending)
	}
}

func testStopCancels(t *testing.T, h async.Handle) {
	if isClosed(h.Done()) {
		t.Fatal("Done closed before Stop in a task that runs until stopped")
	}
	ctx, cancel := context.WithTimeout(t.Context(), stopTimeout)
	defer cancel()
	// Readers and stoppers race the Stop, so -race catches a Handle that
	// is not safe for concurrent use.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			// Bounded, so a Handle whose Done never closes fails below
			// with its own message instead of hanging the test.
			select {
			case <-h.Done():
			case <-time.After(stopTimeout):
				return
			}
			_, _ = h.Outcome()
			_ = h.Progress()
		})
		wg.Go(func() { _ = h.Stop(ctx) })
	}
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	waitDone(t, h, stopTimeout, "Stop")
	wg.Wait()
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("second Stop = %v, want nil (a no-op once Done)", err)
	}
	out := outcome(t, h)
	if out.Status != async.StatusCancelled {
		t.Fatalf("Status = %q, want %q (outcome %+v)", out.Status, async.StatusCancelled, out)
	}
	if out.Pending != nil {
		t.Errorf("Pending = %+v on a cancelled task, want nil", out.Pending)
	}
}

func testSendAfterDone(t *testing.T, h async.Handle) {
	waitDone(t, h, episodeTimeout, "the start")
	ctx, cancel := context.WithTimeout(t.Context(), stopTimeout)
	defer cancel()
	if err := h.Send(ctx, "one more thing"); !errors.Is(err, async.ErrFinished) {
		t.Fatalf("Send after Done = %v, want an error wrapping async.ErrFinished", err)
	}
}

func testWaitOutcome(t *testing.T, h async.Handle) {
	waitDone(t, h, episodeTimeout, "the start")
	out := outcome(t, h)
	if out.Status != async.StatusWaiting {
		t.Fatalf("Status = %q, want %q (outcome %+v)", out.Status, async.StatusWaiting, out)
	}
	if out.Pending == nil || len(out.Pending.Requests) == 0 {
		t.Fatalf("Pending = %+v, want an interrupt with at least one request", out.Pending)
	}
	ctx, cancel := context.WithTimeout(t.Context(), stopTimeout)
	defer cancel()
	if err := h.Send(ctx, "go on"); !errors.Is(err, async.ErrFinished) {
		t.Errorf("Send to a waiting episode = %v, want an error wrapping async.ErrFinished", err)
	}
}

func testContinueReturnsNewHandle(t *testing.T, h async.Handle) {
	c, ok := h.(async.Continuer)
	if !ok {
		t.Skip("the Handle does not implement async.Continuer")
	}
	waitDone(t, h, episodeTimeout, "the start")
	first := outcome(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), episodeTimeout)
	defer cancel()
	next, err := c.Continue(ctx, "one more thing")
	if err != nil {
		t.Fatalf("Continue = %v", err)
	}
	if next == nil {
		t.Fatal("Continue returned a nil Handle and no error")
	}
	stopLater(t, next)
	if next == h {
		t.Fatal("Continue returned the receiver; want a new Handle")
	}
	if !isClosed(h.Done()) {
		t.Fatal("the first episode's Done reopened")
	}
	if again := outcome(t, h); !sameOutcome(first, again) {
		t.Fatalf("the first episode's Outcome changed: %+v, then %+v", first, again)
	}
	if !isClosed(next.Done()) {
		if out, ok := next.Outcome(); ok {
			t.Fatalf("new Outcome = %+v, true before its Done closed", out)
		}
	}
	waitDone(t, next, episodeTimeout, "Continue")
	if out := outcome(t, next); out.Status != async.StatusCompleted {
		t.Fatalf("the next episode's Status = %q, want %q (outcome %+v)", out.Status, async.StatusCompleted, out)
	}
}

func testContinueOnce(t *testing.T, h async.Handle) {
	c, ok := h.(async.Continuer)
	if !ok {
		t.Skip("the Handle does not implement async.Continuer")
	}
	waitDone(t, h, episodeTimeout, "the start")
	ctx, cancel := context.WithTimeout(t.Context(), episodeTimeout)
	defer cancel()
	once(t, func() (async.Handle, error) { return c.Continue(ctx, "one more thing") }, async.ErrFinished, "Continue")
}

func testResolveReturnsNewHandle(t *testing.T, h async.Handle) {
	r, ok := h.(async.Resolver)
	if !ok {
		t.Skip("the Handle does not implement async.Resolver")
	}
	waitDone(t, h, episodeTimeout, "the start")
	first := outcome(t, h)
	if first.Pending == nil {
		t.Fatalf("waiting Outcome without Pending: %+v", first)
	}
	decisions := make([]agent.Decision, len(first.Pending.Requests))
	for i, req := range first.Pending.Requests {
		decisions[i] = agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Approve}
	}
	ctx, cancel := context.WithTimeout(t.Context(), episodeTimeout)
	defer cancel()
	next, err := r.Resolve(ctx, decisions)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if next == nil {
		t.Fatal("Resolve returned a nil Handle and no error")
	}
	stopLater(t, next)
	if next == h {
		t.Fatal("Resolve returned the receiver; want a new Handle")
	}
	if !isClosed(h.Done()) {
		t.Fatal("the waiting episode's Done reopened")
	}
	if again := outcome(t, h); !sameOutcome(first, again) {
		t.Fatalf("the waiting episode's Outcome changed: %+v, then %+v", first, again)
	}
	waitDone(t, next, episodeTimeout, "Resolve")
	if out := outcome(t, next); out.Status != async.StatusCompleted {
		t.Fatalf("the resolved episode's Status = %q, want %q (outcome %+v)", out.Status, async.StatusCompleted, out)
	}
}

func testResolveOnce(t *testing.T, h async.Handle) {
	r, ok := h.(async.Resolver)
	if !ok {
		t.Skip("the Handle does not implement async.Resolver")
	}
	waitDone(t, h, episodeTimeout, "the start")
	out := outcome(t, h)
	if out.Pending == nil {
		t.Fatalf("waiting Outcome without Pending: %+v", out)
	}
	decisions := make([]agent.Decision, len(out.Pending.Requests))
	for i, req := range out.Pending.Requests {
		decisions[i] = agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Reject, Message: "not now"}
	}
	ctx, cancel := context.WithTimeout(t.Context(), episodeTimeout)
	defer cancel()
	once(t, func() (async.Handle, error) { return r.Resolve(ctx, decisions) }, async.ErrNotWaiting, "Resolve")
}

// once calls next twice at once, then a third time, and checks that exactly
// one call returned a new Handle and the others failed with an error
// wrapping loserErr.
func once(t *testing.T, next func() (async.Handle, error), loserErr error, what string) {
	t.Helper()
	type res struct {
		h   async.Handle
		err error
	}
	results := make([]res, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			h, err := next()
			results[i] = res{h, err}
		})
	}
	wg.Wait()
	h, err := next()
	results = append(results, res{h, err})
	wins := 0
	for _, r := range results {
		if r.err == nil && r.h != nil {
			wins++
			stopLater(t, r.h)
			continue
		}
		if r.h != nil {
			stopLater(t, r.h)
		}
		if !errors.Is(r.err, loserErr) {
			t.Errorf("a repeated %s = %v, want an error wrapping %v", what, r.err, loserErr)
		}
	}
	if wins != 1 {
		t.Fatalf("%d of 3 %s calls on one Handle succeeded, want 1", wins, what)
	}
}
