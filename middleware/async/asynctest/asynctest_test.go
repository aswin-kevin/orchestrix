package asynctest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/middleware/async"
)

// fake is a minimal correct Handle: an episode the test ends by hand.
type fake struct {
	done chan struct{}
	once sync.Once

	mu  sync.Mutex
	out async.Outcome
}

func newFake() *fake { return &fake{done: make(chan struct{})} }

// end publishes out and closes done, once.
func (f *fake) end(out async.Outcome) {
	f.once.Do(func() {
		f.mu.Lock()
		f.out = out
		f.mu.Unlock()
		close(f.done)
	})
}

func (f *fake) Done() <-chan struct{} { return f.done }

func (f *fake) Outcome() (async.Outcome, bool) {
	select {
	case <-f.done:
	default:
		return async.Outcome{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out, true
}

func (f *fake) Progress() string { return "" }

func (f *fake) Send(context.Context, string) error {
	select {
	case <-f.done:
		return async.ErrFinished
	default:
		return nil
	}
}

func (f *fake) Stop(context.Context) error {
	f.end(async.Outcome{Status: async.StatusCancelled})
	return nil
}

// pending is the interrupt of a waiting fake.
var pending = &agent.Interrupt{Requests: []agent.ActionRequest{{ToolUseID: "w1", Tool: "write_file"}}}

// startFake starts a correct fake task for sc.
func startFake(_ *testing.T, sc Scenario) *fake {
	f := newFake()
	switch sc {
	case ScenarioComplete:
		f.end(async.Outcome{Status: async.StatusCompleted, Text: "done"})
	case ScenarioWait:
		f.end(async.Outcome{Status: async.StatusWaiting, Pending: pending})
	}
	return f
}

func TestRunFakeRunner(t *testing.T) {
	Run(t, func(t *testing.T, sc Scenario) async.Handle { return startFake(t, sc) })
}

func TestNilHandleSkips(t *testing.T) {
	Run(t, func(t *testing.T, sc Scenario) async.Handle {
		if sc == ScenarioWait {
			return nil
		}
		return startFake(t, sc)
	})
}

// brokenEnv makes a re-executed test binary run the suite against a broken
// fake.
const brokenEnv = "ORX_ASYNCTEST_BROKEN"

// override replaces one method of a correct fake.
type override struct {
	*fake
	outcome func() (async.Outcome, bool)
	send    func(context.Context, string) error
	stop    func(context.Context) error
	doneCh  func() <-chan struct{}
}

func (o *override) Done() <-chan struct{} {
	if o.doneCh != nil {
		return o.doneCh()
	}
	return o.fake.Done()
}

func (o *override) Outcome() (async.Outcome, bool) {
	if o.outcome != nil {
		return o.outcome()
	}
	return o.fake.Outcome()
}

func (o *override) Send(ctx context.Context, msg string) error {
	if o.send != nil {
		return o.send(ctx, msg)
	}
	return o.fake.Send(ctx, msg)
}

func (o *override) Stop(ctx context.Context) error {
	if o.stop != nil {
		return o.stop(ctx)
	}
	return o.fake.Stop(ctx)
}

// mutant runs the suite against fakes that break(f, sc) changes.
func mutant(brk func(o *override, sc Scenario)) func(t *testing.T) {
	return func(t *testing.T) {
		Run(t, func(t *testing.T, sc Scenario) async.Handle {
			o := &override{fake: startFake(t, sc)}
			brk(o, sc)
			return o
		})
	}
}

// mutants maps the kit case each mutant must fail to a runner of the suite
// against a fake that breaks exactly that behaviour.
var mutants = map[string]func(t *testing.T){
	// Outcome reports ok before Done is closed.
	"OutcomeBeforeDone": mutant(func(o *override, _ Scenario) {
		o.outcome = func() (async.Outcome, bool) {
			o.mu.Lock()
			defer o.mu.Unlock()
			return o.out, true
		}
	}),
	// A Stop after Done reopens the episode with a new channel (which the
	// next Stop closes, so the suite's cleanup does not wait for it).
	"DoneStaysClosed": mutant(func(o *override, _ Scenario) {
		var mu sync.Mutex
		var reopenedCh chan struct{}
		var ch <-chan struct{} = o.fake.Done()
		reopened := false
		o.doneCh = func() <-chan struct{} {
			mu.Lock()
			defer mu.Unlock()
			return ch
		}
		o.stop = func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case reopened:
				close(reopenedCh)
				reopened = false
			case isClosed(ch):
				reopenedCh, reopened = make(chan struct{}), true
				ch = reopenedCh
			}
			return o.fake.Stop(ctx)
		}
	}),
	// A completed task reports a pending interrupt.
	"CompleteOutcome": mutant(func(o *override, sc Scenario) {
		if sc == ScenarioComplete {
			o.outcome = func() (async.Outcome, bool) {
				out, ok := o.fake.Outcome()
				out.Pending = pending
				return out, ok
			}
		}
	}),
	// A stopped task ends failed instead of cancelled.
	"StopCancels": mutant(func(o *override, _ Scenario) {
		o.stop = func(context.Context) error {
			o.end(async.Outcome{Status: async.StatusFailed, Error: "killed"})
			return nil
		}
	}),
	// Send accepts messages after Done.
	"SendAfterDone": mutant(func(o *override, _ Scenario) {
		o.send = func(context.Context, string) error { return nil }
	}),
	// A waiting task reports no pending interrupt.
	"WaitOutcome": mutant(func(o *override, sc Scenario) {
		if sc == ScenarioWait {
			o.outcome = func() (async.Outcome, bool) {
				out, ok := o.fake.Outcome()
				out.Pending = nil
				return out, ok
			}
		}
	}),
}

// TestBrokenFakesAreFlagged proves each case of the suite catches the
// behaviour it checks: every mutant must fail its own case. The mutants run
// in a re-executed test binary, since a failing suite fails its test.
func TestBrokenFakesAreFlagged(t *testing.T) {
	if name := os.Getenv(brokenEnv); name != "" {
		mutants[name](t)
		return
	}
	for name := range mutants {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrokenFakesAreFlagged$", "-test.v")
			cmd.Env = append(os.Environ(), brokenEnv+"="+name)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the suite passed a broken fake:\n%s", out)
			}
			if !strings.Contains(string(out), "--- FAIL: TestBrokenFakesAreFlagged/"+name+" (") {
				t.Fatalf("the suite did not flag case %s:\n%s", name, out)
			}
		})
	}
}
