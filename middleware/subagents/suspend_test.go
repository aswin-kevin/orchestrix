package subagents

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

type writeFileInput struct {
	Text string `json:"text"`
}

// approvalWriter is a write_file tool that needs approval (it declares no
// permission of its own) and writes /approved.md to the call's backend.
func approvalWriter(writes *atomic.Int32) tool.Tool {
	return tool.New("write_file", "Writes a file.", func(ctx context.Context, in writeFileInput, c *tool.Call) (tool.Result, error) {
		writes.Add(1)
		if err := c.Backend.Write(ctx, "/approved.md", []byte(in.Text)); err != nil {
			return tool.Result{}, err
		}
		return tool.Text("wrote it"), nil
	})
}

// threadBackends is a BackendFactory giving each thread its own backend, so a
// run that resolves its backend from its own thread id writes elsewhere.
type threadBackends struct {
	mu       sync.Mutex
	backends map[string]*memory.Backend
}

func (f *threadBackends) get(threadID string) backend.Backend {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.backends == nil {
		f.backends = map[string]*memory.Backend{}
	}
	if f.backends[threadID] == nil {
		f.backends[threadID] = memory.New()
	}
	return f.backends[threadID]
}

func (f *threadBackends) threads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.backends {
		ids = append(ids, id)
	}
	return ids
}

// interruptFixture is a parent, running with no Approver, whose writer
// subagent asks to call write_file.
type interruptFixture struct {
	parent, child *llmtest.Scripted
	agent         *agent.Agent
	backends      *threadBackends
	writes        *atomic.Int32
}

func newInterruptFixture(t *testing.T, approver permission.Approver) *interruptFixture {
	t.Helper()
	f := &interruptFixture{
		child: llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w1", Name: "write_file", Input: obj{"text": "hello"}}}},
			llmtest.Turn{Text: "child finished"},
		),
		parent: llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "writer", "write hello")}},
			llmtest.Turn{Text: "parent done"},
		),
		backends: &threadBackends{},
		writes:   &atomic.Int32{},
	}
	mw := newMiddleware(t, Config{Subagents: []Subagent{{
		Name: "writer", Description: "Writes.", SystemPrompt: "you write", Provider: f.child,
		Tools: []tool.Tool{approvalWriter(f.writes)},
	}}})
	a, err := agent.New(agent.Config{
		Provider:       f.parent,
		Middleware:     []agent.Middleware{mw},
		BackendFactory: f.backends.get,
		Approver:       approver,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	f.agent = a
	return f
}

// interrupt runs the parent until the child's request surfaces and returns
// the parent's state after a JSON round trip, as a host would store it.
func (f *interruptFixture) interrupt(t *testing.T) *state.State {
	t.Helper()
	events, final := collect(t, context.Background(), f.agent, userInput("go"))
	if final.Reason != agent.ReasonInterrupted {
		t.Fatalf("reason = %s (%v), want interrupted", final.Reason, final.Err)
	}
	var interrupts []agent.Event
	for _, ev := range events {
		if ev.Kind == agent.KindInterrupt {
			interrupts = append(interrupts, ev)
		}
	}
	if len(interrupts) != 1 || interrupts[0].ParentToolID != "" {
		t.Fatalf("interrupt events = %+v, want exactly the parent's own", interrupts)
	}
	pending, ok := agent.Pending(final.State)
	if !ok || len(pending.Requests) != 1 {
		t.Fatalf("pending = %+v, want one request", pending)
	}
	req := pending.Requests[0]
	if req.ToolUseID != "t1" || req.Tool != ToolTask ||
		!strings.HasPrefix(req.Reason, "Subagent writer needs approval: write_file(") ||
		!strings.Contains(req.Reason, `{"text":"hello"}`) {
		t.Fatalf("pending request = %+v, want the task call naming write_file", req)
	}
	if !slices.Equal(req.Allowed, []agent.DecisionType{agent.Approve, agent.Reject}) {
		t.Fatalf("allowed = %v, want approve and reject", req.Allowed)
	}
	if f.writes.Load() != 0 {
		t.Fatal("write_file ran before approval")
	}
	data, err := json.Marshal(final.State)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	restored := state.New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	return restored
}

func (f *interruptFixture) resume(t *testing.T, s *state.State, d agent.Decision) *agent.Final {
	t.Helper()
	var final *agent.Final
	for ev := range f.agent.Resume(context.Background(), "thread", s, []agent.Decision{d}) {
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final.Reason != agent.ReasonCompleted || final.Text != "parent done" {
		t.Fatalf("resumed parent = %s %q (%v), want completed with parent done", final.Reason, final.Text, final.Err)
	}
	return final
}

func TestChildInterruptSurfacesAndResumes(t *testing.T) {
	f := newInterruptFixture(t, nil)
	s := f.interrupt(t)
	final := f.resume(t, s, agent.Decision{ToolUseID: "t1", Type: agent.Approve})

	if n := f.writes.Load(); n != 1 {
		t.Fatalf("write_file ran %d times, want once", n)
	}
	if res := onlyResult(t, f.parent); res.IsError || blockText(res) != "child finished" {
		t.Fatalf("task result = %q (error=%v), want the child's final text", blockText(res), res.IsError)
	}
	// The resumed child wrote to the parent's backend, not one of its own.
	got, err := f.backends.get("thread").Read(context.Background(), "/approved.md", 0, 0)
	if err != nil || !slices.Equal(got.Lines, []string{"hello"}) {
		t.Fatalf("parent backend /approved.md = %+v, %v; want the child's file", got, err)
	}
	if ids := f.backends.threads(); !slices.Equal(ids, []string{"thread"}) {
		t.Fatalf("backends were created for threads %v, want only the parent's", ids)
	}
	if entries, _ := suspendedKey.Get(final.State); len(entries) != 0 {
		t.Fatalf("suspended entries = %v, want none once resumed", entries)
	}
}

func TestChildInterruptReject(t *testing.T) {
	f := newInterruptFixture(t, nil)
	s := f.interrupt(t)
	final := f.resume(t, s, agent.Decision{ToolUseID: "t1", Type: agent.Reject, Message: "no"})

	if n := f.writes.Load(); n != 0 {
		t.Fatalf("write_file ran %d times after a rejection", n)
	}
	if f.child.Calls() != 2 {
		t.Fatalf("child made %d requests, want 2: the rejection must reach it and it must continue", f.child.Calls())
	}
	results := toolResults(f.child.Requests[1])
	if len(results) != 1 || !results[0].IsError || blockText(results[0]) != "User rejected this action: no" {
		t.Fatalf("child write result = %+v, want the rejection", results)
	}
	if res := onlyResult(t, f.parent); blockText(res) != "child finished" {
		t.Fatalf("task result = %q, want the child's final text", blockText(res))
	}
	if entries, _ := suspendedKey.Get(final.State); len(entries) != 0 {
		t.Fatalf("suspended entries = %v, want none once rejected", entries)
	}
}

func TestChildUsesParentApprover(t *testing.T) {
	var asked atomic.Int32
	f := newInterruptFixture(t, func(context.Context, permission.Request, string) permission.Decision {
		asked.Add(1)
		return permission.Allowed()
	})
	_, final := collect(t, context.Background(), f.agent, userInput("go"))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed without an interrupt", final.Reason, final.Err)
	}
	if asked.Load() != 1 || f.writes.Load() != 1 {
		t.Fatalf("approver asked %d times, write_file ran %d times; want 1 and 1", asked.Load(), f.writes.Load())
	}
}

func TestResumeWithoutSuspendedChild(t *testing.T) {
	f := newInterruptFixture(t, nil)
	s := f.interrupt(t)
	suspendedKey.Delete(s)
	f.resume(t, s, agent.Decision{ToolUseID: "t1", Type: agent.Approve})

	res := onlyResult(t, f.parent)
	if !res.IsError || !strings.Contains(blockText(res), "no suspended subagent") {
		t.Fatalf("task result = %q (error=%v), want the missing-entry error", blockText(res), res.IsError)
	}
	if f.child.Calls() != 1 || f.writes.Load() != 0 {
		t.Fatalf("child requests = %d, writes = %d; want the child left alone", f.child.Calls(), f.writes.Load())
	}
}
