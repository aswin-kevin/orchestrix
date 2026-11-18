package agent

import (
	"errors"
	"fmt"
	"testing"
)

func TestInterruptIsError(t *testing.T) {
	var err error = &Interrupt{Requests: []ActionRequest{{ToolUseID: "t1"}}}
	wrapped := fmt.Errorf("run: %w", err)

	var got *Interrupt
	if !errors.As(wrapped, &got) || len(got.Requests) != 1 {
		t.Fatalf("errors.As failed: %v", wrapped)
	}
	if want := "agent: interrupted: 1 action(s) awaiting a decision"; got.Error() != want {
		t.Fatalf("Error() = %q, want %q", got.Error(), want)
	}
}
