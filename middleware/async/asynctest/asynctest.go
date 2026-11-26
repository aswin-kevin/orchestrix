// Package asynctest holds the conformance suite every async.Runner and the
// async.Handle values it returns must pass. It is a regular (non-test) file
// so that other packages' tests can import it; nothing outside tests should.
//
// The suite checks the frozen Handle contract: a Handle covers one episode of
// a task, from its start until it ends or starts waiting; Done closes when
// the episode stops and stays closed; Outcome reports ok only once Done is
// closed, with Pending set exactly when the task is waiting; Stop cancels a
// running episode and is a no-op once Done; Send fails with
// async.ErrFinished once Done.
package asynctest

import (
	"context"
	"errors"
	"testing"
	"time"

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

// EpisodeTimeout bounds how long the suite waits for a ScenarioComplete or
// ScenarioWait episode to stop.
const EpisodeTimeout = 30 * time.Second

// StopTimeout bounds how long the suite waits for a stopped episode to end.
const StopTimeout = 5 * time.Second

// Run executes the conformance suite, one subtest per check, each with tasks
// of its own from start. Every Handle start returns that is still running
// when its subtest ends is stopped then.
func Run(t *testing.T, start StartFunc) {
	t.Helper()
	cases := []struct {
		name string
		sc   Scenario
		fn   func(t *testing.T, h async.Handle)
	}{
		{"OutcomeBeforeDone", ScenarioRunUntilStopped, testOutcomeBeforeDone},
		{"DoneStaysClosed", ScenarioComplete, testDoneStaysClosed},
		{"CompleteOutcome", ScenarioComplete, testCompleteOutcome},
		{"StopCancels", ScenarioRunUntilStopped, testStopCancels},
		{"SendAfterDone", ScenarioComplete, testSendAfterDone},
		{"WaitOutcome", ScenarioWait, testWaitOutcome},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, c.sc)
			if h == nil {
				t.Skipf("the runner does not support scenario %s", c.sc)
			}
			t.Cleanup(func() {
				if isClosed(h.Done()) {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), StopTimeout)
				defer cancel()
				_ = h.Stop(ctx)
				select {
				case <-h.Done():
				case <-ctx.Done():
				}
			})
			c.fn(t, h)
		})
	}
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

func testDoneStaysClosed(t *testing.T, h async.Handle) {
	waitDone(t, h, EpisodeTimeout, "the start")
	first := outcome(t, h)
	// Stop is a no-op once Done: the episode must not reopen nor change.
	ctx, cancel := context.WithTimeout(context.Background(), StopTimeout)
	defer cancel()
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop after Done = %v, want nil", err)
	}
	for i := range 3 {
		if !isClosed(h.Done()) {
			t.Fatalf("Done reopened (read %d after it closed)", i+1)
		}
	}
	if again := outcome(t, h); again.Status != first.Status || again.Text != first.Text || again.Error != first.Error {
		t.Fatalf("Outcome changed after Done: %+v, then %+v", first, again)
	}
}

func testCompleteOutcome(t *testing.T, h async.Handle) {
	waitDone(t, h, EpisodeTimeout, "the start")
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
	ctx, cancel := context.WithTimeout(context.Background(), StopTimeout)
	defer cancel()
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	waitDone(t, h, StopTimeout, "Stop")
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
	waitDone(t, h, EpisodeTimeout, "the start")
	ctx, cancel := context.WithTimeout(context.Background(), StopTimeout)
	defer cancel()
	if err := h.Send(ctx, "one more thing"); !errors.Is(err, async.ErrFinished) {
		t.Fatalf("Send after Done = %v, want an error wrapping async.ErrFinished", err)
	}
}

func testWaitOutcome(t *testing.T, h async.Handle) {
	waitDone(t, h, EpisodeTimeout, "the start")
	out := outcome(t, h)
	if out.Status != async.StatusWaiting {
		t.Fatalf("Status = %q, want %q (outcome %+v)", out.Status, async.StatusWaiting, out)
	}
	if out.Pending == nil || len(out.Pending.Requests) == 0 {
		t.Fatalf("Pending = %+v, want an interrupt with at least one request", out.Pending)
	}
	ctx, cancel := context.WithTimeout(context.Background(), StopTimeout)
	defer cancel()
	if err := h.Send(ctx, "go on"); !errors.Is(err, async.ErrFinished) {
		t.Errorf("Send to a waiting episode = %v, want an error wrapping async.ErrFinished", err)
	}
}
