package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

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
