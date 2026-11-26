package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/permission"
)

// activeApprover returns an approver whose terminal is owned by a turn, as
// during a run, so it asks instead of denying.
func activeApprover(in *lineReader, out io.Writer) *approver {
	ap := newApprover(in, out)
	ap.beginTurn()
	return ap
}

func TestApproverCancelledWhileWaitingDenies(t *testing.T) {
	pr, pw := io.Pipe() // never written to: the prompt would block forever
	defer pw.Close()
	ap := activeApprover(newLineReader(pr), &bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan permission.Decision, 1)
	go func() { done <- ap.decide(ctx, permission.Request{Tool: "write_file"}, "") }()

	select {
	case d := <-done:
		if d.Behavior != permission.Deny || d.Message != deniedCancelled {
			t.Errorf("decision = %+v, want deny %q", d, deniedCancelled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decide did not return after cancellation")
	}
}

func TestApproverEndOfInputDenies(t *testing.T) {
	ap := activeApprover(newLineReader(strings.NewReader("")), &bytes.Buffer{})

	d := ap.decide(context.Background(), permission.Request{Tool: "write_file"}, "")

	if d.Behavior != permission.Deny || d.Message != deniedByUser {
		t.Errorf("decision = %+v, want deny %q", d, deniedByUser)
	}
}

// promptLog is an io.Writer safe for the concurrent prompts of the test.
type promptLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *promptLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *promptLog) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), s)
}

func (l *promptLog) prompts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), "Allow ")
}

// waitPrompts waits until the log holds n prompts.
func waitPrompts(t *testing.T, l *promptLog, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for l.prompts() < n {
		if time.Now().After(deadline) {
			t.Fatalf("saw %d prompts, want %d", l.prompts(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestApproverSerializesConcurrentPrompts(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	out := &promptLog{}
	ap := activeApprover(newLineReader(pr), out)

	decisions := make(chan permission.Decision, 2)
	for _, tool := range []string{"write_file", "edit_file"} {
		go func() { decisions <- ap.decide(context.Background(), permission.Request{Tool: tool}, "") }()
	}
	waitPrompts(t, out, 1)
	time.Sleep(50 * time.Millisecond) // room for a second, unserialized prompt
	if n := out.prompts(); n != 1 {
		t.Fatalf("%d prompts shown before the first was answered, want 1", n)
	}
	fmt.Fprintln(pw, "y")
	waitPrompts(t, out, 2)
	fmt.Fprintln(pw, "n")

	allowed := 0
	for range 2 {
		if d := <-decisions; d.Behavior == permission.Allow {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("%d calls allowed, want exactly the one answered y", allowed)
	}
}

func TestPromptNamesSubagent(t *testing.T) {
	tests := []struct {
		name string
		info agent.RunInfo
		ok   bool
		want string
	}{
		{"outside a run", agent.RunInfo{}, false, ""},
		{"top-level run", agent.RunInfo{AgentName: "main"}, true, ""},
		{"subagent", agent.RunInfo{AgentName: "researcher", Depth: 1}, true, "[researcher] "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := askerLabel(tc.info, tc.ok); got != tc.want {
				t.Errorf("askerLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApproverNeverRemembersDisableRememberRequests(t *testing.T) {
	out := &promptLog{}
	ap := activeApprover(newLineReader(strings.NewReader("y\ny\na\n")), out)
	// A tool other than exit_plan_mode, which has a prompt of its own.
	req := permission.Request{Tool: "deploy", DisableRemember: true}

	if d := ap.decide(context.Background(), req, ""); d.Behavior != permission.Allow {
		t.Fatalf("first = %+v, want allow", d)
	}
	if d := ap.decide(context.Background(), req, ""); d.Behavior != permission.Allow {
		t.Fatalf("second = %+v, want allow", d)
	}
	if d := ap.decide(context.Background(), req, ""); d.Behavior != permission.Deny {
		t.Fatalf("third = %+v, want deny: a is not offered, so it does not allow", d)
	}
	if got := out.prompts(); got != 3 {
		t.Fatalf("prompts = %d, want 3", got)
	}
	if strings.Contains(out.buf.String(), "/a]") {
		t.Fatalf("prompt offers always for a DisableRemember request: %q", out.buf.String())
	}
	if ap.isAlways("deploy") {
		t.Fatal("always recorded")
	}
}

func TestApproverAlwaysStillWorksForOrdinaryRequests(t *testing.T) {
	out := &promptLog{}
	ap := activeApprover(newLineReader(strings.NewReader("a\n")), out)
	req := permission.Request{Tool: "write_file"}
	ap.decide(context.Background(), req, "")
	ap.decide(context.Background(), req, "")
	if got := out.prompts(); got != 1 {
		t.Fatalf("prompts = %d, want 1", got)
	}
}

func TestApproverIgnoresEarlierAlwaysForDisableRemember(t *testing.T) {
	ap := activeApprover(newLineReader(strings.NewReader("n\n")), &promptLog{})
	ap.allowAlways("exit_plan_mode")
	d := ap.decide(context.Background(), permission.Request{Tool: "exit_plan_mode", DisableRemember: true}, "")
	if d.Behavior != permission.Deny {
		t.Fatalf("decision = %+v, want the prompt to run and deny", d)
	}
}

func TestApproverEscapesSubject(t *testing.T) {
	out := &promptLog{}
	ap := activeApprover(newLineReader(strings.NewReader("n\n")), out)

	ap.decide(context.Background(), permission.Request{Tool: "execute", Subject: "ls\x1b[2K\rrm -rf /"}, "")

	got := out.buf.String()
	if strings.ContainsAny(got, "\x1b\r") {
		t.Errorf("prompt contains raw control characters: %q", got)
	}
	if want := `Allow execute ls\x1b[2K\rrm -rf /? [y/N/a] `; !strings.Contains(got, want) {
		t.Errorf("prompt = %q, want it to contain %q", got, want)
	}
}

// planRequest is the request planmode sends for exit_plan_mode.
func planRequest(plan string) permission.Request {
	return permission.Request{Tool: "exit_plan_mode", Subject: plan, DisableRemember: true}
}

func TestPlanPromptShowsFullPlan(t *testing.T) {
	out := &promptLog{}
	ap := activeApprover(newLineReader(strings.NewReader("y\n")), out)

	d := ap.decide(context.Background(), planRequest("1. read\n\t2. write\x1b]0;owned\x07"), "")

	if d.Behavior != permission.Allow {
		t.Fatalf("decision = %+v, want allow", d)
	}
	got := out.buf.String()
	if want := "1. read\n\t2. write\\x1b]0;owned\\x07\n"; !strings.Contains(got, want) {
		t.Errorf("output = %q, want the escaped plan %q", got, want)
	}
	if !strings.HasSuffix(got, "Approve plan? [y/N] ") {
		t.Errorf("output = %q, want it to end with the plan prompt", got)
	}
	if strings.Contains(got, "Allow ") {
		t.Errorf("output = %q, want the plan prompt instead of the generic one", got)
	}
}

func TestPlanPromptRejectionReadsFeedback(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"feedback", "n\nsplit step two\n", "split step two"},
		{"blank feedback", "n\n\n", deniedByUser},
		{"end of input", "n\n", deniedByUser},
		{"always is not offered", "a\nno\n", "no"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := &promptLog{}
			ap := activeApprover(newLineReader(strings.NewReader(tc.input)), out)

			d := ap.decide(context.Background(), planRequest("step"), "")

			if d.Behavior != permission.Deny || d.Message != tc.want {
				t.Errorf("decision = %+v, want deny %q", d, tc.want)
			}
			if !strings.Contains(out.buf.String(), "Feedback") {
				t.Errorf("output = %q, want a feedback prompt", out.buf.String())
			}
			if ap.isAlways("exit_plan_mode") {
				t.Error("always recorded for the plan")
			}
		})
	}
}

func TestPlanPromptRefusesCutPlan(t *testing.T) {
	out := &promptLog{}
	ap := activeApprover(newLineReader(strings.NewReader("y\n")), out)

	d := ap.decide(context.Background(), planRequest("step one"+planmode.TruncationMarker), "")

	if d.Behavior != permission.Deny || d.Message != deniedPlanTooLong {
		t.Errorf("decision = %+v, want deny %q", d, deniedPlanTooLong)
	}
	if got := out.buf.String(); !strings.Contains(got, "warning") || strings.Contains(got, planPrompt) {
		t.Errorf("output = %q, want a warning and no approval prompt", got)
	}
}

func TestPlanPromptCancelled(t *testing.T) {
	t.Run("at the approval", func(t *testing.T) {
		pr, pw := io.Pipe()
		defer pw.Close()
		ap := activeApprover(newLineReader(pr), &promptLog{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if d := ap.decide(ctx, planRequest("step"), ""); d.Behavior != permission.Deny || d.Message != deniedCancelled {
			t.Errorf("decision = %+v, want deny %q", d, deniedCancelled)
		}
	})
	t.Run("at the feedback", func(t *testing.T) {
		pr, pw := io.Pipe()
		defer pw.Close()
		out := &promptLog{}
		ap := activeApprover(newLineReader(pr), out)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan permission.Decision, 1)
		go func() { done <- ap.decide(ctx, planRequest("step"), "") }()

		fmt.Fprintln(pw, "n")
		deadline := time.Now().Add(2 * time.Second)
		for !out.contains(feedbackPrompt) {
			if time.Now().After(deadline) {
				t.Fatal("no feedback prompt")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()

		select {
		case d := <-done:
			if d.Behavior != permission.Deny || d.Message != deniedCancelled {
				t.Errorf("decision = %+v, want deny %q", d, deniedCancelled)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("decide did not return after cancellation")
		}
	})
}

func TestApprovalWhileIdleDeniedWithoutPrompt(t *testing.T) {
	out := &promptLog{}
	// no turn runs
	ap := newApprover(newLineReader(strings.NewReader("y\n")), out)

	d := ap.decide(context.Background(), permission.Request{Tool: "write_file", Subject: "/x"}, "")

	if d.Behavior != permission.Deny || d.Message != deniedNoTerminal {
		t.Errorf("decision = %+v, want a denial %q", d, deniedNoTerminal)
	}
	if out.buf.Len() != 0 {
		t.Errorf("printed %q while idle, want nothing", out.buf.String())
	}
}
