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
	req := permission.Request{Tool: "exit_plan_mode", NoRemember: true}

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
	if ap.isAlways("exit_plan_mode") {
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
