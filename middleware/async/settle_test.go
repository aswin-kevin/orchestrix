package async

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/agenttest"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logged gives h's parent a logger writing to the returned buffer.
func logged(t *testing.T, h *harness) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	a, err := agent.New(agent.Config{
		Provider: h.p, Middleware: []agent.Middleware{h.m}, PermissionMode: permission.ModeDefault,
		Logger: slog.New(slog.NewTextHandler(buf, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.a = a
	return buf
}

// eventually waits until cond holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOnSettledResolvesWaitingTask(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var mp atomic.Pointer[Middleware]
	var resolveErr atomic.Pointer[error]
	hook := func(ctx context.Context, threadID string, rec TaskRecord) {
		if rec.Status != StatusWaiting {
			return
		}
		err := mp.Load().Resolve(ctx, threadID, rec.ID, []agent.Decision{{ToolUseID: "w1", Type: agent.Approve}})
		resolveErr.Store(&err)
	}
	w := newWriter(llmtest.Turn{Text: "wrote it"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{w.spec()}, OnSettled: hook, DisableStopReminder: true},
		start("s1", "researcher", "write"), llmtest.Turn{Text: "started"}, llmtest.Turn{Text: "noted"})
	mp.Store(h.m)
	h.be = testutil.Memory(t)
	h.run(t, context.Background(), "go", nil)
	eventually(t, "the decision", func() bool { return resolveErr.Load() != nil })
	if err := *resolveErr.Load(); err != nil {
		t.Fatalf("Resolve from OnSettled = %v", err)
	}
	eventually(t, "the task's end", func() bool { return w.runs.Load() == 1 && liveRecord(t, h, "async_1").terminal() })
	h.run(t, context.Background(), "next", nil)
	if r := record(t, h.st, "async_1"); r.Status != StatusCompleted || r.Result != "wrote it" || !r.Notified {
		t.Fatalf("record = %+v, want completed and announced", r)
	}
	var ends int
	for _, n := range notifications(h.p) {
		if strings.Contains(n.Text(), "completed.") {
			ends++
		}
	}
	if ends != 1 {
		t.Fatalf("%d completion notices, want 1", ends)
	}
}

func TestSlowOnSettledDoesNotDelayClose(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hook := func(_ context.Context, _ string, rec TaskRecord) {
		if rec.Status == StatusCancelled {
			<-release
		}
	}
	hd := ended(waitingOutcome())
	run := funcRunner(func(context.Context, Request) (Handle, error) { return hd, nil })
	h := newHarness(t, Config{Runner: run, OnSettled: hook, DisableStopReminder: true},
		start("s1", gp, "x"), llmtest.Turn{Text: "started"})
	h.run(t, context.Background(), "go", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	_ = h.m.Close(ctx)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("Close took %v behind a blocked OnSettled", took)
	}
}

// askTwice is a tool that asks for a decision every time it runs, the
// resumed run included.
var askTwice = tool.New("ask", "Asks.", func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
	return tool.Result{}, &agent.Interrupt{}
})

func TestSameCallWaitingAgainIsAnnounced(t *testing.T) {
	spec := researcher(llmtest.New(call("w1", "ask", obj{})))
	spec.Tools = []tool.Tool{askTwice}
	h := newHarness(t, Config{Subagents: []subagents.Subagent{spec}, DisableStopReminder: true},
		start("s1", "researcher", "x"), llmtest.Turn{Text: "started"}, llmtest.Turn{Text: "noted"}, llmtest.Turn{Text: "again"})
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	h.run(t, context.Background(), "next", nil) // announces the wait
	r := record(t, h.st, "async_1")
	if r.Status != StatusWaiting || !r.Notified {
		t.Fatalf("record = %+v", r)
	}
	if err := h.m.Resolve(context.Background(), "t", "async_1", approveAll(t, r)); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	eventually(t, "the second wait", func() bool {
		out, _, ok := h.m.jobs.Get("t", "async_1").Handle.outcome()
		return ok && out.Status == StatusWaiting
	})
	h.run(t, context.Background(), "and now?", nil)
	var waits int
	for _, n := range notifications(h.p) {
		if strings.Contains(n.Text(), "is waiting for a decision") {
			waits++
		}
	}
	if waits != 2 {
		t.Fatalf("%d waiting notices, want 2: the same call waits again after the decision", waits)
	}
}

func TestEndNestedCancelsClaimedWaitingTask(t *testing.T) {
	hd := ended(waitingOutcome())
	run := funcRunner(func(context.Context, Request) (Handle, error) { return hd, nil })
	h := newHarness(t, Config{Runner: run, DisableStopReminder: true},
		start("s1", gp, "x"), llmtest.Turn{Text: "started"}, llmtest.Turn{Text: "noted"})
	h.run(t, context.Background(), "go", nil)
	h.run(t, context.Background(), "next", nil) // announces the wait
	// A decision claimed the task and has not committed its episode yet.
	tk := h.m.jobs.Get("t", "async_1").Handle
	tk.mu.Lock()
	tk.state = episodeClaimed
	tk.mu.Unlock()
	ctx, _ := agenttest.Context(context.Background(), agent.RunInfo{ThreadID: "t", Depth: 1})
	if err := h.m.AfterAgent(ctx, h.st); err != nil {
		t.Fatal(err)
	}
	if r := record(t, h.st, "async_1"); r.Status != StatusCancelled || !r.Notified {
		t.Fatalf("record = %+v, want cancelled and seen", r)
	}
}

func TestTaskStoppedAsItStartedIsNotReported(t *testing.T) {
	calls := &settledCalls{}
	entered, proceed := make(chan struct{}), make(chan struct{})
	hd := newCtlHandle(Outcome{Status: StatusCancelled})
	runner := funcRunner(func(context.Context, Request) (Handle, error) {
		close(entered)
		<-proceed
		return hd, nil
	})
	h := newHarness(t, Config{Runner: runner, OnSettled: calls.hook}, start("s1", gp, "d"), llmtest.Turn{Text: "x"})
	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		<-entered
		stopped := make(chan struct{})
		go func() { _ = h.m.Close(context.Background()); close(stopped) }()
		defer func() { <-stopped }()
		eventually(t, "the stop", func() bool { return h.m.jobs.Get("t", "async_1").Stopped() })
		close(proceed)
	}()
	h.run(t, context.Background(), "go", nil)
	<-shutdown
	<-hd.Done()
	time.Sleep(20 * time.Millisecond) // a report would run on its own goroutine
	if got := calls.get(); len(got) != 0 {
		t.Fatalf("OnSettled = %+v for a task the parent never recorded", got)
	}
}

func TestOnSettledPanicIsLogged(t *testing.T) {
	var called atomic.Bool
	hook := func(context.Context, string, TaskRecord) { called.Store(true); panic("boom") }
	child := llmtest.New(llmtest.Turn{Text: "done"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(child)}, OnSettled: hook, DisableStopReminder: true},
		start("s1", "researcher", "x"), llmtest.Turn{Text: "started"})
	buf := logged(t, h)
	h.run(t, context.Background(), "go", nil)
	eventually(t, "OnSettled", called.Load)
	eventually(t, "the log line", func() bool { return strings.Contains(buf.String(), "async: OnSettled panicked") })
	if !strings.Contains(buf.String(), "boom") {
		t.Fatalf("log = %s, want the panic value", buf.String())
	}
}

func TestOnEventPanicIsRecovered(t *testing.T) {
	var calls atomic.Int32
	onEvent := func(string, agent.Event) { calls.Add(1); panic("boom") }
	child := llmtest.New(llmtest.Turn{Text: "done"})
	h := newHarness(t, Config{Subagents: []subagents.Subagent{researcher(child)}, OnEvent: onEvent, DisableStopReminder: true},
		start("s1", "researcher", "x"), llmtest.Turn{Text: "started"})
	buf := logged(t, h)
	h.run(t, context.Background(), "go", nil)
	waitEnded(t, h.m, "async_1")
	if out := outcomeOf(t, h.m, h.st, "async_1"); out.Status != StatusCompleted || out.Text != "done" {
		t.Fatalf("outcome = %+v, want the task completed despite the panics", out)
	}
	if calls.Load() < 2 {
		t.Fatalf("OnEvent called %d times, want every event", calls.Load())
	}
	if !strings.Contains(buf.String(), "async: OnEvent panicked") {
		t.Fatalf("log = %s, want the recovered panic", buf.String())
	}
}
