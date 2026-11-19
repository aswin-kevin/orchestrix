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

func TestApproverCancelledWhileWaitingDenies(t *testing.T) {
	pr, pw := io.Pipe() // never written to: the prompt would block forever
	defer pw.Close()
	ap := newApprover(newLineReader(pr), &bytes.Buffer{})
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
	ap := newApprover(newLineReader(strings.NewReader("")), &bytes.Buffer{})

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
	ap := newApprover(newLineReader(pr), out)

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

func TestApproverNeverRemembersNoRememberRequests(t *testing.T) {
	out := &promptLog{}
	ap := newApprover(newLineReader(strings.NewReader("y\ny\na\n")), out)
	// A tool other than exit_plan_mode, which has a prompt of its own.
	req := permission.Request{Tool: "deploy", NoRemember: true}

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
		t.Fatalf("prompt offers always for a NoRemember request: %q", out.buf.String())
	}
	if ap.isAlways("deploy") {
		t.Fatal("always recorded")
	}
}

func TestApproverAlwaysStillWorksForOrdinaryRequests(t *testing.T) {
	out := &promptLog{}
	ap := newApprover(newLineReader(strings.NewReader("a\n")), out)
	req := permission.Request{Tool: "write_file"}
	ap.decide(context.Background(), req, "")
	ap.decide(context.Background(), req, "")
	if got := out.prompts(); got != 1 {
		t.Fatalf("prompts = %d, want 1", got)
	}
}

func TestApproverIgnoresEarlierAlwaysForNoRemember(t *testing.T) {
	ap := newApprover(newLineReader(strings.NewReader("n\n")), &promptLog{})
	ap.allowAlways("exit_plan_mode")
	d := ap.decide(context.Background(), permission.Request{Tool: "exit_plan_mode", NoRemember: true}, "")
	if d.Behavior != permission.Deny {
		t.Fatalf("decision = %+v, want the prompt to run and deny", d)
	}
}

func TestTermSafe(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain text", "ls -la", "ls -la"},
		{"newline and tab kept", "a\n\tb", "a\n\tb"},
		{"escape sequence", "\x1b[31mred", `\x1b[31mred`},
		{"carriage return", "safe\rrm -rf /", `safe\rrm -rf /`},
		{"bell and delete", "a\x07b\x7f", `a\x07b\x7f`},
		{"C1 control", "a\u009b31m", `a\u009b31m`},
		{"bidi override", "a\u202eb", `a\u202eb`},
		{"invalid UTF-8", "a\x9b31mb", `a\x9b31mb`},
		{"raw C1 byte", "\x85", `\x85`},
		{"cut multi-byte sequence", "a\xe2\x80", `a\xe2\x80`},
		{"arabic letter mark", "a\u061cb", `a\u061cb`},
		{"zero-width space", "a\u200bb", `a\u200bb`},
		{"zero-width joiner", "a\u200db", `a\u200db`},
		{"word joiner", "a\u2060b", `a\u2060b`},
		{"byte order mark", "\ufeffa", `\ufeffa`},
		{"unicode text kept", "héllo ✓", "héllo ✓"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := termSafe(tc.in); got != tc.want {
				t.Errorf("termSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApproverEscapesSubject(t *testing.T) {
	out := &promptLog{}
	ap := newApprover(newLineReader(strings.NewReader("n\n")), out)

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
	return permission.Request{Tool: "exit_plan_mode", Subject: plan, NoRemember: true}
}

func TestPlanPromptShowsFullPlan(t *testing.T) {
	out := &promptLog{}
	ap := newApprover(newLineReader(strings.NewReader("y\n")), out)

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
			ap := newApprover(newLineReader(strings.NewReader(tc.input)), out)

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
	ap := newApprover(newLineReader(strings.NewReader("y\n")), out)

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
		ap := newApprover(newLineReader(pr), &promptLog{})
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
		ap := newApprover(newLineReader(pr), out)
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
