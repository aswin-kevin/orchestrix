//go:build unix

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

type obj = map[string]any

// pidCommand prints the shell's pid and then becomes a long sleep with the
// same pid, so a test can check that exactly that process is gone.
const pidCommand = "echo $$; exec sleep 1000"

func newShell(t *testing.T) *shell.Backend {
	t.Helper()
	b, err := shell.New(t.TempDir(), shell.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newJobs returns a middleware that is closed when the test ends.
func newJobs(t *testing.T, cfg Config) *Middleware {
	t.Helper()
	m := mustNew(t, cfg)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func newAgent(t *testing.T, p llm.Provider, m *Middleware, b *shell.Backend, extra ...tool.Tool) *agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Config{
		Provider:       p,
		Backend:        b,
		Tools:          extra,
		Middleware:     []agent.Middleware{mustFS(t, filesystem.Config{Execute: true, Background: true}), m},
		PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func run(t *testing.T, ctx context.Context, a *agent.Agent, in agent.Input) *agent.Final {
	t.Helper()
	var final *agent.Final
	for ev := range a.Run(ctx, in) {
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatal("run ended without a final event")
	}
	if final.Err != nil {
		t.Fatalf("run failed: %v (%s)", final.Err, final.Reason)
	}
	return final
}

func input(thread string) agent.Input {
	return agent.Input{ThreadID: thread, Messages: []llm.Message{llm.UserText("go")}}
}

func call(id, name string, in obj) llmtest.Call { return llmtest.Call{ID: id, Name: name, Input: in} }

func turn(calls ...llmtest.Call) llmtest.Turn { return llmtest.Turn{ToolCalls: calls} }

func bgExecute(id, command string) llmtest.Call {
	return call(id, filesystem.ToolExecute, obj{"command": command, "run_in_background": true})
}

// pidMonitor waits until the job has printed its pid.
func pidMonitor(id, task string) llmtest.Call {
	return call(id, ToolMonitor, obj{"job_id": task, "pattern": `^[0-9]+$`, "timeout_seconds": 10})
}

// result returns the tool_result block answering the call id in any request.
func result(t *testing.T, p *llmtest.Scripted, id string) llm.Block {
	t.Helper()
	for _, req := range p.Requests {
		for _, msg := range req.Messages {
			for _, b := range msg.Content {
				if b.Type == llm.BlockToolResult && b.ToolUseID == id {
					return b
				}
			}
		}
	}
	t.Fatalf("no tool result for %s", id)
	return llm.Block{}
}

func resultText(t *testing.T, p *llmtest.Scripted, id string) string {
	t.Helper()
	var parts []string
	for _, c := range result(t, p, id).Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

// pidFrom reads the pid a pidMonitor call matched: the last line of its result.
func pidFrom(t *testing.T, p *llmtest.Scripted, id string) int {
	t.Helper()
	text := resultText(t, p, id)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		t.Fatalf("no pid in monitor result %q", text)
	}
	return pid
}

func alive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// waitGone polls until pid has exited, failing the test after a few seconds.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// notifications returns the texts of the task notifications in req.
func notifications(req llm.Request) []string {
	var out []string
	for _, msg := range req.Messages {
		if strings.Contains(msg.Text(), "<job-notification>") {
			out = append(out, msg.Text())
		}
	}
	return out
}

func TestBackgroundExecuteAndNotification(t *testing.T) {
	m := newJobs(t, Config{})
	p := llmtest.New(
		turn(bgExecute("e1", "sleep 0.2; echo done")),
		turn(call("s1", ToolSleep, obj{"seconds": 5, "reason": "waiting for the build"})),
		turn(call("o1", ToolCheck, obj{"job_id": "bg_1"})),
		llmtest.Turn{Text: "ok"},
	)
	start := time.Now()
	final := run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))

	if got := resultText(t, p, "e1"); !strings.Contains(got, "Started background job bg_1.") {
		t.Fatalf("execute result = %q", got)
	}
	if got := resultText(t, p, "s1"); got != "woke: job bg_1 finished" {
		t.Fatalf("sleep result = %q", got)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("sleep did not return early (run took %s)", elapsed)
	}
	want := "<job-notification>Background job bg_1 (`sleep 0.2; echo done`) completed (exit code 0). Use check_job to read its output.</job-notification>"
	if got := notifications(p.Requests[2]); len(got) != 1 || got[0] != want {
		t.Fatalf("turn 3 notifications = %q, want [%q]", got, want)
	}
	if got := notifications(p.Requests[3]); len(got) != 1 {
		t.Fatalf("notification repeated or lost: %q", got)
	}
	out := resultText(t, p, "o1")
	if !strings.Contains(out, "done") || !strings.Contains(out, "exit code 0") || !strings.Contains(out, "completed") {
		t.Fatalf("check_job = %q", out)
	}

	var meta string
	for _, msg := range final.State.Messages() {
		if id, ok := msg.Meta[NotificationMetaKey]; ok {
			meta = id
		}
	}
	if meta != "bg_1" {
		t.Fatalf("notification meta = %q, want bg_1", meta)
	}
	records := Records(final.State)
	if len(records) != 1 || records[0].Status != StatusCompleted || !records[0].Notified || records[0].ThreadID != "t1" {
		t.Fatalf("records = %+v", records)
	}
}

func TestCancelJobKillsJob(t *testing.T) {
	m := newJobs(t, Config{})
	p := llmtest.New(
		turn(bgExecute("e1", pidCommand)),
		turn(pidMonitor("m1", "bg_1")),
		turn(call("x1", ToolCancel, obj{"job_id": "bg_1"})),
		turn(call("o1", ToolCheck, obj{"job_id": "bg_1"}), call("l1", ToolList, obj{})),
		llmtest.Turn{Text: "ok"},
	)
	final := run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))

	pid := pidFrom(t, p, "m1")
	if alive(pid) {
		t.Fatalf("process %d survived cancel_job", pid)
	}
	if got := resultText(t, p, "o1"); !strings.Contains(got, "cancelled") {
		t.Fatalf("check_job after stop = %q", got)
	}
	if got := resultText(t, p, "l1"); !strings.Contains(got, "bg_1") || !strings.Contains(got, "cancelled") {
		t.Fatalf("list_jobs = %q", got)
	}
	for _, req := range p.Requests {
		if got := notifications(req); len(got) != 0 {
			t.Fatalf("a cancelled job was announced again: %q", got)
		}
	}
	records := Records(final.State)
	if len(records) != 1 || records[0].Status != StatusCancelled {
		t.Fatalf("records = %+v", records)
	}
}

func TestCancelJobAllowsItself(t *testing.T) {
	m := mustNew(t, Config{})
	for _, tl := range m.Tools() {
		d := selfCheck(tl, context.Background(), json.RawMessage(`{}`), permission.Context{})
		readOnly := isReadOnly(tl, json.RawMessage(`{}`))
		switch tl.Name() {
		case ToolCancel:
			if d.Behavior != permission.Allow || readOnly {
				t.Fatalf("cancel_job: decision %v, read-only %v", d.Behavior, readOnly)
			}
		default:
			if !readOnly || !isConcurrent(tl, json.RawMessage(`{}`)) {
				t.Fatalf("%s must be read-only and concurrent", tl.Name())
			}
		}
	}
}

func TestMaxRunningEnforced(t *testing.T) {
	m := newJobs(t, Config{MaxRunning: 1})
	p := llmtest.New(
		turn(bgExecute("e1", "sleep 1000"), bgExecute("e2", "sleep 1000")),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))

	if got := resultText(t, p, "e1"); !strings.Contains(got, "bg_1") {
		t.Fatalf("first start = %q", got)
	}
	second := result(t, p, "e2")
	if !second.IsError || !strings.Contains(resultText(t, p, "e2"), "limit 1") {
		t.Fatalf("second start = %q (error %v)", resultText(t, p, "e2"), second.IsError)
	}
}

func TestMonitorPattern(t *testing.T) {
	m := newJobs(t, Config{})
	p := llmtest.New(
		turn(bgExecute("e1", "for i in 1 2 3; do echo tick$i; sleep 0.1; done")),
		turn(call("m1", ToolMonitor, obj{"job_id": "bg_1", "pattern": "tick2", "timeout_seconds": 10})),
		turn(call("m2", ToolMonitor, obj{"job_id": "bg_1", "pattern": "("})),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))

	got := resultText(t, p, "m1")
	if !strings.Contains(got, "Pattern matched in job bg_1 (still running)") || !strings.HasSuffix(got, "tick2") {
		t.Fatalf("monitor result = %q", got)
	}
	if bad := result(t, p, "m2"); !bad.IsError {
		t.Fatalf("invalid pattern accepted: %q", resultText(t, p, "m2"))
	}
}

func TestLostJobAfterRestart(t *testing.T) {
	s := state.New()
	recordsKey.Set(s, []Record{{ID: "bg_1", ThreadID: "t1", Command: "make serve", Status: StatusRunning, Started: time.Now()}})
	m := newJobs(t, Config{})
	p := llmtest.New(
		turn(bgExecute("e1", "true")),
		llmtest.Turn{Text: "ok"},
	)
	in := input("t1")
	in.State = s
	final := run(t, context.Background(), newAgent(t, p, m, newShell(t)), in)

	want := "<job-notification>Background job bg_1 (`make serve`) lost (exit code -1). Use check_job to read its output.</job-notification>"
	if got := notifications(p.Requests[0]); len(got) != 1 || got[0] != want {
		t.Fatalf("notifications = %q, want [%q]", got, want)
	}
	if got := resultText(t, p, "e1"); !strings.Contains(got, "bg_2") {
		t.Fatalf("a new job reused a recorded id: %q", got)
	}
	records := Records(final.State)
	if len(records) != 2 || records[0].Status != StatusLost || !records[0].Notified {
		t.Fatalf("records = %+v", records)
	}
}

// settledGoroutines polls until the goroutine count drops to at most want,
// returning the last count seen.
func settledGoroutines(want int) int {
	deadline := time.Now().Add(5 * time.Second)
	n := runtime.NumGoroutine()
	for n > want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

func TestCloseKillsAll(t *testing.T) {
	baseline := runtime.NumGoroutine()
	m := mustNew(t, Config{})
	b := newShell(t)
	var pids []int
	for _, thread := range []string{"t1", "t2"} {
		p := llmtest.New(
			turn(bgExecute("e1", pidCommand)),
			turn(pidMonitor("m1", "bg_1")),
			llmtest.Turn{Text: "ok"},
		)
		run(t, context.Background(), newAgent(t, p, m, b), input(thread))
		pids = append(pids, pidFrom(t, p, "m1"))
	}
	for _, pid := range pids {
		if !alive(pid) {
			t.Fatalf("job %d of a finished top-level run did not keep running", pid)
		}
	}

	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, pid := range pids {
		if alive(pid) {
			t.Fatalf("process %d survived Close", pid)
		}
	}
	if n := settledGoroutines(baseline); n > baseline {
		t.Fatalf("goroutines after Close = %d, baseline %d", n, baseline)
	}

	// A closed middleware starts nothing new.
	p := llmtest.New(turn(bgExecute("e1", "true")), llmtest.Turn{Text: "ok"})
	run(t, context.Background(), newAgent(t, p, m, b), input("t3"))
	if r := result(t, p, "e1"); !r.IsError {
		t.Fatalf("start after Close = %q", resultText(t, p, "e1"))
	}
}

// spawnInput is the input of the test tool that runs a nested agent.
type spawnInput struct{}

func spawnTool(child *agent.Agent) tool.Tool {
	return tool.New("spawn", "Runs a nested agent.", func(ctx context.Context, _ spawnInput, _ *tool.Call) (tool.Result, error) {
		for ev := range child.Run(ctx, input("child")) {
			if ev.Kind == agent.KindFinal && ev.Final.Err != nil {
				return tool.Errorf("child failed: %v", ev.Final.Err), nil
			}
		}
		return tool.Text("child done"), nil
	})
}

func TestNestedRunStopsOwnJobs(t *testing.T) {
	m := newJobs(t, Config{})
	b := newShell(t)
	childP := llmtest.New(
		turn(bgExecute("ce1", pidCommand)),
		turn(pidMonitor("cm1", "bg_1")),
		llmtest.Turn{Text: "child ok"},
	)
	child := newAgent(t, childP, m, b)
	parentP := llmtest.New(
		turn(bgExecute("pe1", pidCommand)),
		turn(pidMonitor("pm1", "bg_1")),
		turn(call("sp1", "spawn", obj{})),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newAgent(t, parentP, m, b, spawnTool(child)), input("parent"))

	if got := resultText(t, parentP, "sp1"); got != "child done" {
		t.Fatalf("spawn = %q", got)
	}
	childPID, parentPID := pidFrom(t, childP, "cm1"), pidFrom(t, parentP, "pm1")
	if alive(childPID) {
		t.Fatalf("nested run's job %d outlived the run", childPID)
	}
	if !alive(parentPID) {
		t.Fatalf("top-level run's job %d was stopped", parentPID)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitGone(t, parentPID)
}

func TestCheckJobWaitsAndUnknownJob(t *testing.T) {
	m := newJobs(t, Config{})
	p := llmtest.New(
		turn(bgExecute("e1", "sleep 0.2; echo late")),
		turn(call("o1", ToolCheck, obj{"job_id": "bg_1", "wait": true, "timeout_seconds": 10}),
			call("o2", ToolCheck, obj{"job_id": "bg_9"})),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))

	if got := resultText(t, p, "o1"); !strings.Contains(got, "Job bg_1 (`sleep 0.2; echo late`): completed (exit code 0)") || !strings.Contains(got, "late") {
		t.Fatalf("check_job with wait = %q, want the ended job with its output", got)
	}
	if r := result(t, p, "o2"); !r.IsError {
		t.Fatalf("unknown job accepted: %q", resultText(t, p, "o2"))
	}
}

func TestNotificationIsMarkedInjected(t *testing.T) {
	msg := notification(Record{ID: "bg_1", Command: "true", Status: StatusCompleted})
	if msg.Meta[NotificationMetaKey] != "bg_1" || msg.Meta[agent.InjectedMetaKey] != Name {
		t.Fatalf("notification meta = %v, want the job id and the injected mark", msg.Meta)
	}
}

// isReadOnly reports whether tool.As finds t read-only for input in.
func isReadOnly(t tool.Tool, in json.RawMessage) bool {
	r, ok := tool.As[tool.ReadOnlyer](t)
	return ok && r.ReadOnly(in)
}

// isConcurrent reports whether tool.As finds t concurrency-safe for input in.
func isConcurrent(t tool.Tool, in json.RawMessage) bool {
	c, ok := tool.As[tool.ConcurrencySafer](t)
	return ok && c.ConcurrencySafe(in)
}

// selfCheck is t's own permission verdict, found with tool.As; the zero
// Decision when t has none.
func selfCheck(t tool.Tool, ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision {
	c, ok := tool.As[tool.PermissionChecker](t)
	if !ok {
		return permission.Decision{}
	}
	return c.CheckPermission(ctx, in, pc)
}

func TestCancelJobRecordsCancelled(t *testing.T) {
	m := newJobs(t, Config{})
	p := llmtest.New(
		turn(bgExecute("e1", "sleep 30")),
		turn(call("x1", ToolCancel, obj{"job_id": "bg_1"})),
		llmtest.Turn{Text: "ok"},
	)
	final := run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))

	if got := resultText(t, p, "x1"); got != "Cancelled job bg_1." {
		t.Fatalf("cancel_job = %q", got)
	}
	records := Records(final.State)
	if len(records) != 1 || records[0].Status != StatusCancelled || !records[0].Notified {
		t.Fatalf("records = %+v", records)
	}
}
