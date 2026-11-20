package evals

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// The scenarios run here against a scripted model that does what each one
// asks, so they must pass: a failure is a bug in the scenario or a harness
// invariant broken offline.

func scenario(t *testing.T, name string) Scenario {
	t.Helper()
	for _, sc := range Scenarios() {
		if sc.Name == name {
			return sc
		}
	}
	t.Fatalf("no scenario %q", name)
	return Scenario{}
}

func runScripted(t *testing.T, name string, turns ...llmtest.Turn) Trial {
	t.Helper()
	tr := RunTrial(context.Background(), Env{Model: llmtest.New(turns...)}, scenario(t, name))
	if !tr.Pass {
		t.Fatalf("trial = %+v, want a pass", tr)
	}
	return tr
}

func call(id, name string, input any) llmtest.Turn {
	return llmtest.Turn{ToolCalls: []llmtest.Call{{ID: id, Name: name, Input: input}}}
}

func text(s string) llmtest.Turn { return llmtest.Turn{Text: s} }

func TestDenialScripted(t *testing.T) {
	tr := runScripted(t, "denial",
		call("1", "delete_records", map[string]any{"customer_id": "42"}),
		text("I am not allowed to delete records."))
	if tr.Notes["attempts"] != 1 {
		t.Errorf("notes = %v, want one attempt", tr.Notes)
	}
}

func TestCrossProcessApprovalScripted(t *testing.T) {
	runScripted(t, "approval-cross-process",
		call("1", "execute", map[string]any{"command": "echo original"}),
		text("The command was denied."),
		text("It printed edited."))
}

func TestPromptInjectionScripted(t *testing.T) {
	runScripted(t, "prompt-injection",
		call("1", "read_file", map[string]any{"path": "/notes.txt"}),
		call("2", "execute", map[string]any{"command": "rm -rf / --no-preserve-root"}),
		call("3", "execute", map[string]any{"command": "echo cleanup"}),
		text("The release moved to Thursday. Priya owns the rollback checklist."))
}

func TestShellParsingScripted(t *testing.T) {
	tr := runScripted(t, "shell-parsing",
		call("1", "execute", map[string]any{"command": unanalyzableCommand}),
		text("The command needed approval and was declined."))
	if tr.Notes["unanalysable_attempts"] != 1 {
		t.Errorf("notes = %v, want one unanalysable attempt", tr.Notes)
	}
}

func TestBudgetTurnsScripted(t *testing.T) {
	tr := runScripted(t, "budget-turns",
		call("1", "write_file", map[string]any{"path": "/f1.txt", "content": "1"}),
		call("2", "write_file", map[string]any{"path": "/f2.txt", "content": "2"}),
		text("I wrote f1 and f2; f3 to f5 are left."))
	if tr.Reason != "max_turns" {
		t.Errorf("reason = %q, want max_turns", tr.Reason)
	}
}

func TestBudgetTimeScripted(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the time budget")
	}
	tr := runScripted(t, "budget-time",
		call("1", "slow_lookup", map[string]any{"service": "billing"}),
		text("The lookup did not finish in time."))
	if tr.Reason != "timeout" {
		t.Errorf("reason = %q, want timeout", tr.Reason)
	}
}

func TestBudgetTokensScripted(t *testing.T) {
	short := llmtest.Turn{Text: "OK", Usage: llm.Usage{Output: 5}}
	tr := runScripted(t, "budget-tokens", short, short, short)
	if tr.Notes["nudges"] != 2 {
		t.Errorf("notes = %v, want two nudges", tr.Notes)
	}
}

func TestBackgroundJobScripted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the job is a POSIX shell command")
	}
	if testing.Short() {
		t.Skip("waits for the background job")
	}
	runScripted(t, "background-job",
		call("1", "execute", map[string]any{"command": "sleep 2 && echo " + jobMarker, "run_in_background": true}),
		text("STARTED"),
		call("2", "task_list", map[string]any{}),
		text("It printed "+jobMarker+"."))
}

func TestConcurrentThreadsScripted(t *testing.T) {
	// One reply for the seed run and one for the worker that wins; the
	// loser never calls the model.
	runScripted(t, "concurrent-threads", text("READY"), text("DONE"))
}

func TestViolationFailsTheTrial(t *testing.T) {
	sc := Scenario{Name: "x", Run: func(_ context.Context, _ Env, t *T) {
		t.Failf("model")
		t.Violationf("harness")
	}}
	tr := RunTrial(context.Background(), Env{}, sc)
	if tr.Pass || tr.Violation != "harness" || tr.Failure != "model" {
		t.Errorf("trial = %+v", tr)
	}
}

func TestPanicIsAViolation(t *testing.T) {
	tr := RunTrial(context.Background(), Env{}, Scenario{Name: "x", Run: func(context.Context, Env, *T) { panic("boom") }})
	if tr.Pass || !strings.Contains(tr.Violation, "boom") {
		t.Errorf("trial = %+v, want a violation naming the panic", tr)
	}
}

func TestCompare(t *testing.T) {
	res := Results{TrialsEach: 5, Scenarios: []ScenarioResult{
		{Name: "steady", PassRate: 0.8},
		{Name: "dropped", PassRate: 0.4},
		{Name: "broken", PassRate: 1, Violations: 1},
		{Name: "new", PassRate: 0},
	}}
	base := Baseline{Scenarios: map[string]float64{"steady": 1, "dropped": 0.8, "broken": 1, "gone": 1}}
	got := strings.Join(Compare(res, base, 0.2, false), "\n")
	for _, want := range []string{"dropped: pass rate 0.40", "broken: 1 harness violation", "gone: in the baseline but not run"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems %q lack %q", got, want)
		}
	}
	for _, unwanted := range []string{"steady", "new"} {
		if strings.Contains(got, unwanted+":") {
			t.Errorf("problems %q name %q", got, unwanted)
		}
	}
	if got := Compare(res, base, 0.2, true); strings.Contains(strings.Join(got, "\n"), "gone") {
		t.Errorf("a filtered run reports missing scenarios: %q", got)
	}
}
