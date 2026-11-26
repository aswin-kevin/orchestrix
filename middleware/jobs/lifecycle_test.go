//go:build unix

package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	registry "github.com/aswin-kevin/orchestrix/internal/jobs"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/state"
)

// threadCount reports how many threads the middleware keeps an entry for.
func threadCount(m *Middleware) int {
	return m.jobs.ThreadCount()
}

func TestThreadlessRunHasNoJobs(t *testing.T) {
	m := newJobs(t, Config{})
	p := llmtest.New(
		turn(bgExecute("e1", "sleep 1000")),
		turn(call("l1", ToolList, obj{}), call("o1", ToolCheck, obj{"job_id": "bg_1"})),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newAgent(t, p, m, newShell(t)), input(""))

	if r := result(t, p, "e1"); !r.IsError || !strings.Contains(resultText(t, p, "e1"), "background jobs need a thread id") {
		t.Fatalf("threadless start = %q (error %v)", resultText(t, p, "e1"), r.IsError)
	}
	if got := resultText(t, p, "l1"); got != "No background jobs." {
		t.Fatalf("threadless list_jobs = %q", got)
	}
	if r := result(t, p, "o1"); !r.IsError {
		t.Fatalf("threadless check_job = %q", resultText(t, p, "o1"))
	}
	if n := threadCount(m); n != 0 {
		t.Fatalf("threadless run left %d thread entries", n)
	}
}

func TestSettledThreadIsDropped(t *testing.T) {
	m := newJobs(t, Config{})
	p := llmtest.New(
		turn(bgExecute("e1", "echo hi")),
		turn(call("s1", ToolSleep, obj{"seconds": 5})),
		turn(call("o1", ToolCheck, obj{"job_id": "bg_1"})), // the notification turn
		turn(call("o2", ToolCheck, obj{"job_id": "bg_1"})),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))

	if got := resultText(t, p, "o1"); !strings.Contains(got, "hi") {
		t.Fatalf("output in the notification turn = %q", got)
	}
	if got := resultText(t, p, "o2"); !strings.Contains(got, "completed (exit code 0)") || !strings.Contains(got, "[end of output]\nhi") {
		t.Fatalf("output after release = %q", got)
	}
	if n := threadCount(m); n != 0 {
		t.Fatalf("settled thread kept %d entries", n)
	}

	sleeper := llmtest.New(turn(call("s1", ToolSleep, obj{"seconds": 1})), llmtest.Turn{Text: "ok"})
	run(t, context.Background(), newAgent(t, sleeper, m, newShell(t)), input("t2"))
	if n := threadCount(m); n != 0 {
		t.Fatalf("a sleep-only thread created %d entries", n)
	}
}

func TestStopKeepsStatusOfEndedJob(t *testing.T) {
	m := newJobs(t, Config{})
	p := llmtest.New(
		turn(bgExecute("e1", "exit 3")),
		turn(call("s1", ToolSleep, obj{"seconds": 5})),
		turn(call("x1", ToolCancel, obj{"job_id": "bg_1"}), call("o1", ToolCheck, obj{"job_id": "bg_1"})),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))
	if got := resultText(t, p, "o1"); !strings.Contains(got, "failed (exit code 3)") {
		t.Fatalf("check_job after stopping an ended job = %q", got)
	}
	if stop(context.Background(), nil) == nil {
		t.Fatal("stop accepted a nil job")
	}
}

func TestJobTimeoutCapsRequestedTimeout(t *testing.T) {
	m := newJobs(t, Config{JobTimeout: 300 * time.Millisecond})
	p := llmtest.New(
		turn(call("e1", filesystem.ToolExecute, obj{"command": "sleep 1000", "run_in_background": true, "timeout_seconds": 99999})),
		turn(call("o1", ToolCheck, obj{"job_id": "bg_1", "wait": true, "timeout_seconds": 10})),
		llmtest.Turn{Text: "ok"},
	)
	run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))
	if got := resultText(t, p, "o1"); !strings.Contains(got, "timed_out") {
		t.Fatalf("check_job = %q, want timed_out", got)
	}
}

func TestMaxFinishedEvictsAndPrunes(t *testing.T) {
	m := newJobs(t, Config{MaxFinished: 1})
	p := llmtest.New(
		turn(bgExecute("e1", "echo one")),
		turn(call("s1", ToolSleep, obj{"seconds": 5})),
		turn(bgExecute("e2", "echo two")),
		turn(call("s2", ToolSleep, obj{"seconds": 5})),
		turn(call("o1", ToolCheck, obj{"job_id": "bg_1"}), call("o2", ToolCheck, obj{"job_id": "bg_2"})),
		llmtest.Turn{Text: "ok"},
	)
	final := run(t, context.Background(), newAgent(t, p, m, newShell(t)), input("t1"))

	if r := result(t, p, "o1"); !r.IsError {
		t.Fatalf("evicted task still answered: %q", resultText(t, p, "o1"))
	}
	if got := resultText(t, p, "o2"); !strings.Contains(got, "two") {
		t.Fatalf("latest task output = %q", got)
	}
	records := Records(final.State)
	if len(records) != 1 || records[0].ID != "bg_2" {
		t.Fatalf("records = %+v", records)
	}
}

func TestPruneRecordsKeepsUnsettled(t *testing.T) {
	records := []Record{
		{ID: "bg_1", Status: StatusCompleted, Notified: true},
		{ID: "bg_2", Status: StatusFailed},
		{ID: "bg_3", Status: StatusRunning},
		{ID: "bg_4", Status: StatusCancelled, Notified: true},
		{ID: "bg_5", Status: StatusLost, Notified: true},
	}
	var ids []string
	for _, r := range registry.PruneRecords(records, 1, recordAccessors) {
		ids = append(ids, r.ID)
	}
	if got := strings.Join(ids, ","); got != "bg_2,bg_3,bg_5" {
		t.Fatalf("kept %s, want bg_2,bg_3,bg_5", got)
	}
}

func TestNotificationSurvivesCheckpoint(t *testing.T) {
	m := newJobs(t, Config{})
	b := newShell(t)
	first := llmtest.New(turn(bgExecute("e1", "sleep 0.2; echo hi")), llmtest.Turn{Text: "later"})
	final := run(t, context.Background(), newAgent(t, first, m, b), input("t1"))

	data, err := json.Marshal(final.State)
	if err != nil {
		t.Fatal(err)
	}
	restored := state.New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatal(err)
	}
	j := m.jobs.Get("t1", "bg_1")
	if j == nil {
		t.Fatal("job unknown after the run")
	}
	<-j.Handle.Done()

	second := llmtest.New(turn(call("o1", ToolCheck, obj{"job_id": "bg_1"})), llmtest.Turn{Text: "ok"})
	in := input("t1")
	in.State = restored
	run(t, context.Background(), newAgent(t, second, m, b), in)

	if got := notifications(second.Requests[0]); len(got) != 1 || !strings.Contains(got[0], "bg_1") {
		t.Fatalf("first request notifications = %q", got)
	}
	if got := notifications(second.Requests[1]); len(got) != 1 {
		t.Fatalf("notification delivered %d times", len(got))
	}
	if got := resultText(t, second, "o1"); !strings.Contains(got, "hi") {
		t.Fatalf("check_job = %q", got)
	}
}

func TestOutputTailIsBoundedAndRuneSafe(t *testing.T) {
	if got := outputTail("short"); got != "short" {
		t.Fatalf("short tail = %q", got)
	}
	long := strings.Repeat("é", outputTailBytes) // two bytes per rune
	got := outputTail(long)
	if len(got) > outputTailBytes || !utf8.ValidString(got) || !strings.HasSuffix(long, got) {
		t.Fatalf("tail of %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if odd := outputTail("x" + long); !utf8.ValidString(odd) {
		t.Fatal("tail split a rune")
	}
}
