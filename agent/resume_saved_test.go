package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	cpmemory "github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// savedInterrupt runs, on thread id of an agent with cp as its
// Checkpointer (nil for none), a turn that calls write_file, which the
// default mode pauses, then "done". It requires the run to stop interrupted.
func savedInterrupt(t *testing.T, cp checkpoint.Checkpointer, id string) (*Agent, *llmtest.Scripted, *callLog, *Final) {
	t.Helper()
	log := newCallLog()
	provider := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w")}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: provider, Tools: []tool.Tool{writeTool(log)}, Checkpointer: cp})
	in := userInput("go")
	in.ThreadID = id
	_, final := collect(t, a.Run(t.Context(), in))
	if final.Reason != ReasonInterrupted || final.Err != nil {
		t.Fatalf("Run() final = %s, %v, want interrupted", final.Reason, final.Err)
	}
	return a, provider, log, final
}

// approveAll approves every pending action of s.
func approveAll(t *testing.T, s *state.State) []Decision {
	t.Helper()
	pending, ok := Pending(s)
	if !ok {
		t.Fatal("Pending() = false, want a pending interrupt")
	}
	var ds []Decision
	for _, r := range pending.Requests {
		ds = append(ds, Decision{ToolUseID: r.ToolUseID, Type: Approve})
	}
	return ds
}

func TestResumeWithoutVersionRefusesSavedThread(t *testing.T) {
	ctx := t.Context()
	cp := cpmemory.New()
	a, provider, log, final := savedInterrupt(t, cp, "t1")
	saved := final.Version

	events, got := collect(t, a.Resume(ctx, "t1", final.State, approveAll(t, final.State)))
	if len(events) != 1 {
		t.Fatalf("Resume() yielded %d events, want only KindFinal", len(events))
	}
	if got.Reason != ReasonInvalidResume || !errors.Is(got.Err, ErrResumeWithoutVersion) ||
		!errors.Is(got.Err, checkpoint.ErrConflict) {
		t.Fatalf("Resume() final = %s, %v, want invalid_resume wrapping ErrResumeWithoutVersion and ErrConflict",
			got.Reason, got.Err)
	}
	if got.State != final.State || got.Version != "" {
		t.Fatalf("Resume() final State, Version = %p, %q, want the input State and the zero Version", got.State, got.Version)
	}
	if n := log.count("w"); n != 0 {
		t.Fatalf("write_file ran %d times, want 0", n)
	}
	if n := provider.Calls(); n != 1 {
		t.Fatalf("model calls = %d, want 1", n)
	}
	if _, ok := Pending(final.State); !ok {
		t.Fatal("Pending(state) = false after the refused resume, want the interrupt kept")
	}
	if _, v, err := cp.Load(ctx, "t1"); err != nil || v != saved {
		t.Fatalf("Load() version = %q, %v, want %q unchanged", v, err, saved)
	}

	// The same State, resumed with its Version, completes.
	in := Input{ThreadID: "t1", State: final.State, Version: final.Version}
	if _, got := collect(t, a.ResumeWith(ctx, in, approveAll(t, final.State))); got.Reason != ReasonCompleted {
		t.Fatalf("ResumeWith(Version) final = %s, %v, want completed", got.Reason, got.Err)
	}
	if n := log.count("w"); n != 1 {
		t.Fatalf("write_file ran %d times, want 1", n)
	}
}

func TestThreadResumeOfSavedThreadCompletes(t *testing.T) {
	ctx := t.Context()
	log := newCallLog()
	provider := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w")}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: provider, Tools: []tool.Tool{writeTool(log)}, Checkpointer: cpmemory.New()})
	th := mustThread(t, a, "t1")
	if _, final := collect(t, th.Prompt(ctx, "go")); final.Reason != ReasonInterrupted {
		t.Fatalf("Prompt() final = %s, want interrupted", final.Reason)
	}
	if _, final := collect(t, th.Resume(ctx, approveAll(t, th.State()))); final.Reason != ReasonCompleted {
		t.Fatalf("Thread.Resume() final = %s, %v, want completed", final.Reason, final.Err)
	}
	if n := log.count("w"); n != 1 {
		t.Fatalf("write_file ran %d times, want 1", n)
	}
}

func TestResumeWithoutVersionUnchangedWhenNothingSaved(t *testing.T) {
	tests := []struct {
		name string
		cp   checkpoint.Checkpointer
		// runID is the thread the interrupted run used, resumeID the one
		// it is resumed on.
		runID, resumeID string
	}{
		{"no checkpointer", nil, "t1", "t1"},
		{"fresh thread", cpmemory.New(), "", "fresh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _, log, final := savedInterrupt(t, tt.cp, tt.runID)
			_, got := collect(t, a.Resume(t.Context(), tt.resumeID, final.State, approveAll(t, final.State)))
			if got.Reason != ReasonCompleted {
				t.Fatalf("Resume() final = %s, %v, want completed", got.Reason, got.Err)
			}
			if n := log.count("w"); n != 1 {
				t.Fatalf("write_file ran %d times, want 1", n)
			}
		})
	}
}

// failingLoad is a Checkpointer whose Load fails.
type failingLoad struct{ checkpoint.Checkpointer }

var errLoad = errors.New("load failed")

func (failingLoad) Load(context.Context, string) (*state.State, checkpoint.Version, error) {
	return nil, "", errLoad
}

func TestResumeWithoutVersionLoadError(t *testing.T) {
	a, _, log, final := savedInterrupt(t, failingLoad{cpmemory.New()}, "t1")
	events, got := collect(t, a.Resume(t.Context(), "t1", final.State, approveAll(t, final.State)))
	if len(events) != 1 || got.Reason != ReasonCheckpointError || !errors.Is(got.Err, errLoad) {
		t.Fatalf("Resume() = %d events, final %s, %v, want only checkpoint_error wrapping the load error",
			len(events), got.Reason, got.Err)
	}
	if n := log.count("w"); n != 0 {
		t.Fatalf("write_file ran %d times, want 0", n)
	}
}
