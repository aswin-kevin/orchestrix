package planmode

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

type obj = map[string]any

const planDenied = "Permission denied: plan mode: read-only"

type pathInput struct {
	Path string `json:"path"`
}

// writes counts the write_file calls that actually ran.
type writes struct {
	mu sync.Mutex
	n  int
}

func (w *writes) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// writeFile is an edit tool, so acceptEdits allows it and plan denies it.
func writeFile(w *writes) tool.Tool {
	return tool.New("write_file", "Write a file.", func(_ context.Context, _ pathInput, _ *tool.Call) (tool.Result, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.n++
		return tool.Text("written"), nil
	}, tool.WithEdit())
}

func call(id, name string, input any) llmtest.Call {
	return llmtest.Call{ID: id, Name: name, Input: input}
}

func turn(calls ...llmtest.Call) llmtest.Turn { return llmtest.Turn{ToolCalls: calls} }

func exitCall(plan string) llmtest.Call { return call("x", ToolExit, obj{"plan": plan}) }

func newAgent(t *testing.T, cfg Config, ac agent.Config, turns ...llmtest.Turn) (*agent.Agent, *llmtest.Scripted) {
	t.Helper()
	p := llmtest.New(turns...)
	ac.Provider = p
	ac.Middleware = append(ac.Middleware, mustNew(t, cfg))
	a, err := agent.New(ac)
	if err != nil {
		t.Fatal(err)
	}
	return a, p
}

func drain(t *testing.T, seq func(func(agent.Event, error) bool)) *agent.Final {
	t.Helper()
	var final *agent.Final
	for ev := range seq {
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatal("no final event")
	}
	return final
}

func runToEnd(t *testing.T, a *agent.Agent) *agent.Final {
	t.Helper()
	return drain(t, a.Run(context.Background(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}))
}

// results returns the tool results the model saw, in order, flattened.
func results(p *llmtest.Scripted) []llm.Block {
	var out []llm.Block
	for _, req := range p.Requests {
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == llm.BlockToolResult {
					out = append(out, b)
				}
			}
		}
	}
	return out
}

// resultFor returns the last tool result for the tool_use id.
func resultFor(t *testing.T, p *llmtest.Scripted, id string) llm.Block {
	t.Helper()
	rs := results(p)
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].ToolUseID == id {
			return rs[i]
		}
	}
	t.Fatalf("no result for %q", id)
	return llm.Block{}
}

func text(b llm.Block) string {
	var sb strings.Builder
	for _, c := range b.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

func effectiveMode(a *agent.Agent, s *state.State) permission.Mode { return a.PermissionMode(s) }

func TestEnterFlipsToPlan(t *testing.T) {
	w := &writes{}
	a, p := newAgent(t, Config{},
		agent.Config{PermissionMode: permission.ModeAcceptEdits, Tools: []tool.Tool{writeFile(w)}},
		turn(call("e", ToolEnter, obj{"reason": "big change"})),
		turn(call("w", "write_file", obj{"path": "/a"})),
		llmtest.Turn{Text: "done"},
	)
	final := runToEnd(t, a)
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v)", final.Reason, final.Err)
	}
	if got := text(resultFor(t, p, "w")); !strings.Contains(got, planDenied) {
		t.Fatalf("write result = %q, want %q", got, planDenied)
	}
	if w.count() != 0 {
		t.Fatal("write ran in plan mode")
	}
	if got := effectiveMode(a, final.State); got != permission.ModePlan {
		t.Fatalf("mode = %s", got)
	}
	if !hasSegment(p.Requests[1], DefaultPrompt) {
		t.Fatal("turn 2 lacks the plan-mode segment")
	}
	if hasSegment(p.Requests[0], DefaultPrompt) {
		t.Fatal("turn 1 has the plan-mode segment before entering")
	}
	if !Active(context.Background(), final.State) {
		t.Fatal("Active = false in plan mode")
	}
}

func hasSegment(req llm.Request, seg string) bool { return slices.Contains(req.System, seg) }

func TestEnterTwice(t *testing.T) {
	a, p := newAgent(t, Config{}, agent.Config{},
		turn(call("e1", ToolEnter, obj{})),
		turn(call("e2", ToolEnter, obj{})),
		llmtest.Turn{Text: "done"},
	)
	final := runToEnd(t, a)
	if got := text(resultFor(t, p, "e2")); got != "Already in plan mode." {
		t.Fatalf("second enter = %q", got)
	}
	if prev, _ := previousKey.Get(final.State); prev != permission.ModeDefault {
		t.Fatalf("previous = %q, want default (second enter must not record plan)", prev)
	}
}

func TestExitWhenNotInPlanErrors(t *testing.T) {
	a, p := newAgent(t, Config{}, agent.Config{},
		turn(exitCall("plan")),
		llmtest.Turn{Text: "done"},
	)
	if final := runToEnd(t, a); final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v)", final.Reason, final.Err)
	}
	r := resultFor(t, p, "x")
	if !r.IsError || !strings.Contains(text(r), "not in plan mode") {
		t.Fatalf("result = %+v", r)
	}
}

func TestExitEmptyPlanErrors(t *testing.T) {
	a, p := newAgent(t, Config{DisableEnter: true}, agent.Config{PermissionMode: permission.ModePlan},
		turn(exitCall("  ")),
		llmtest.Turn{Text: "done"},
	)
	runToEnd(t, a)
	if r := resultFor(t, p, "x"); !r.IsError {
		t.Fatalf("empty plan result = %+v, want error", r)
	}
}

func newCheckpointed(t *testing.T, dir string, cfg Config, mode permission.Mode, w *writes, turns ...llmtest.Turn) (*agent.Agent, *llmtest.Scripted) {
	t.Helper()
	cp, err := file.New(dir, file.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return newAgent(t, cfg, agent.Config{
		PermissionMode: mode, Tools: []tool.Tool{writeFile(w)}, Checkpointer: cp,
	}, turns...)
}

func pendingOf(t *testing.T, a *agent.Agent, id string) (*agent.Thread, *agent.Interrupt) {
	t.Helper()
	th, err := a.Thread(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	intr, ok := th.Pending()
	if !ok {
		return th, nil
	}
	return th, intr
}

// interruptedExit starts a thread that enters plan mode and then calls
// exit_plan_mode with plan, returning once it is waiting for a decision.
func interruptedExit(t *testing.T, dir string, cfg Config, mode permission.Mode, plan string) {
	t.Helper()
	a, _ := newCheckpointed(t, dir, cfg, mode, &writes{},
		turn(call("e", ToolEnter, obj{})),
		turn(exitCall(plan)),
	)
	th, err := a.Thread(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	final := drain(t, th.Prompt(context.Background(), "go"))
	if final.Reason != agent.ReasonInterrupted {
		t.Fatalf("reason = %s (%v), want interrupted", final.Reason, final.Err)
	}
	intr, ok := th.Pending()
	if !ok || len(intr.Requests) != 1 {
		t.Fatalf("pending = %+v", intr)
	}
	req := intr.Requests[0]
	want := []agent.DecisionType{agent.Approve, agent.Edit, agent.Reject, agent.Respond}
	if req.Tool != ToolExit || !slices.Equal(req.Allowed, want) {
		t.Fatalf("request = %+v", req)
	}
}

func TestExitInterruptsAndApproveRestores(t *testing.T) {
	dir := t.TempDir()
	interruptedExit(t, dir, Config{}, permission.ModeAcceptEdits, "1. write a.txt")

	w := &writes{}
	a, p := newCheckpointed(t, dir, Config{}, permission.ModeAcceptEdits, w,
		turn(call("w", "write_file", obj{"path": "/a.txt"})),
		llmtest.Turn{Text: "done"},
	)
	th, intr := pendingOf(t, a, "t1")
	if intr == nil {
		t.Fatal("pending interrupt not restored")
	}
	final := drain(t, th.Resume(context.Background(), []agent.Decision{{ToolUseID: "x", Type: agent.Approve}}))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v)", final.Reason, final.Err)
	}
	if got := effectiveMode(a, final.State); got != permission.ModeAcceptEdits {
		t.Fatalf("mode = %s, want acceptEdits", got)
	}
	if plan, _ := Plan(final.State); plan != "1. write a.txt" {
		t.Fatalf("planKey = %q", plan)
	}
	if _, ok := previousKey.Get(final.State); ok {
		t.Fatal("previousKey not deleted")
	}
	if w.count() != 1 {
		t.Fatalf("writes = %d, want 1", w.count())
	}
	got := text(resultFor(t, p, "x"))
	if !strings.Contains(got, "Permission mode is now acceptEdits") || !strings.Contains(got, "Approved plan:\n1. write a.txt") {
		t.Fatalf("approve result = %q", got)
	}
}

func resumeWith(t *testing.T, cfg Config, d agent.Decision) (*agent.Agent, *llmtest.Scripted, *agent.Final) {
	t.Helper()
	dir := t.TempDir()
	interruptedExit(t, dir, cfg, permission.ModeAcceptEdits, "v1")
	a, p := newCheckpointed(t, dir, cfg, permission.ModeAcceptEdits, &writes{}, llmtest.Turn{Text: "done"})
	th, _ := pendingOf(t, a, "t1")
	d.ToolUseID = "x"
	final := drain(t, th.Resume(context.Background(), []agent.Decision{d}))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v)", final.Reason, final.Err)
	}
	return a, p, final
}

func TestRejectStaysInPlanWithFeedback(t *testing.T) {
	a, p, final := resumeWith(t, Config{}, agent.Decision{Type: agent.Reject, Message: "add tests"})
	r := resultFor(t, p, "x")
	if r.IsError || !strings.Contains(text(r), "add tests") {
		t.Fatalf("result = %+v", r)
	}
	if got := effectiveMode(a, final.State); got != permission.ModePlan {
		t.Fatalf("mode = %s, want plan", got)
	}
}

func TestRespondIsFeedback(t *testing.T) {
	a, p, final := resumeWith(t, Config{}, agent.Decision{Type: agent.Respond, Message: "use sqlite"})
	r := resultFor(t, p, "x")
	if r.IsError || !strings.Contains(text(r), "use sqlite") {
		t.Fatalf("result = %+v", r)
	}
	if got := effectiveMode(a, final.State); got != permission.ModePlan {
		t.Fatalf("mode = %s, want plan", got)
	}
}

func TestEditReplacesPlan(t *testing.T) {
	a, _, final := resumeWith(t, Config{}, agent.Decision{Type: agent.Edit, Input: []byte(`{"plan":"v2"}`)})
	if plan, _ := Plan(final.State); plan != "v2" {
		t.Fatalf("planKey = %q, want v2", plan)
	}
	if got := effectiveMode(a, final.State); got != permission.ModeAcceptEdits {
		t.Fatalf("mode = %s", got)
	}
}

func TestApproveModeOverride(t *testing.T) {
	a, _, final := resumeWith(t, Config{ApproveMode: permission.ModeDefault}, agent.Decision{Type: agent.Approve})
	if got := effectiveMode(a, final.State); got != permission.ModeDefault {
		t.Fatalf("mode = %s, want default", got)
	}
	// From bypass.
	dir := t.TempDir()
	interruptedExit(t, dir, Config{ApproveMode: permission.ModeDefault}, permission.ModeBypass, "p")
	a2, _ := newCheckpointed(t, dir, Config{ApproveMode: permission.ModeDefault}, permission.ModeBypass, &writes{}, llmtest.Turn{Text: "done"})
	th, _ := pendingOf(t, a2, "t1")
	f := drain(t, th.Resume(context.Background(), []agent.Decision{{ToolUseID: "x", Type: agent.Approve}}))
	if got := effectiveMode(a2, f.State); got != permission.ModeDefault {
		t.Fatalf("mode from bypass = %s, want default", got)
	}
}

func TestStartedInPlanApprovesToDefault(t *testing.T) {
	cfg := Config{DisableEnter: true}
	mw := mustNew(t, cfg)
	for _, tl := range mw.Tools() {
		if tl.Name() == ToolEnter {
			t.Fatal("DisableEnter still offers enter_plan_mode")
		}
	}
	approver := func(context.Context, permission.Request, string) permission.Decision { return permission.Allowed() }
	a, _ := newAgent(t, cfg, agent.Config{PermissionMode: permission.ModePlan, Approver: approver},
		turn(exitCall("go")),
		llmtest.Turn{Text: "done"},
	)
	final := runToEnd(t, a)
	if got := effectiveMode(a, final.State); got != permission.ModeDefault {
		t.Fatalf("mode = %s, want default", got)
	}
}

func TestApproverSynchronous(t *testing.T) {
	var reasons []string
	var reqs []permission.Request
	run := func(d permission.Decision) (*agent.Agent, *llmtest.Scripted, *agent.Final) {
		approver := func(_ context.Context, req permission.Request, reason string) permission.Decision {
			reasons = append(reasons, reason)
			reqs = append(reqs, req)
			return d
		}
		a, p := newAgent(t, Config{}, agent.Config{PermissionMode: permission.ModeAcceptEdits, Approver: approver},
			turn(call("e", ToolEnter, obj{})),
			turn(exitCall("the plan")),
			llmtest.Turn{Text: "done"},
		)
		return a, p, runToEnd(t, a)
	}
	a, _, final := run(permission.Allowed())
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("allow: reason = %s", final.Reason)
	}
	if got := effectiveMode(a, final.State); got != permission.ModeAcceptEdits {
		t.Fatalf("allow: mode = %s", got)
	}
	if reasons[0] != "Approve this plan and leave plan mode?" || reqs[0].Tool != ToolExit {
		t.Fatalf("approver got %q / %+v", reasons[0], reqs[0])
	}

	a, p, final := run(permission.Decision{Behavior: permission.Deny, Message: "no"})
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("deny: reason = %s", final.Reason)
	}
	if got := effectiveMode(a, final.State); got != permission.ModePlan {
		t.Fatalf("deny: mode = %s", got)
	}
	if r := resultFor(t, p, "x"); r.IsError || !strings.Contains(text(r), "no") {
		t.Fatalf("deny result = %+v", r)
	}
}

func TestSetsPreviousNormalizedToDefault(t *testing.T) {
	a, _ := newAgent(t, Config{}, agent.Config{}, // empty config mode
		turn(call("e", ToolEnter, obj{})),
		llmtest.Turn{Text: "done"},
	)
	final := runToEnd(t, a)
	if prev, _ := previousKey.Get(final.State); prev != permission.ModeDefault {
		t.Fatalf("previous = %q, want default", prev)
	}
}

func TestExitRefusedInSubagent(t *testing.T) {
	child, childP := newAgent(t, Config{}, agent.Config{PermissionMode: permission.ModePlan, Approver: func(context.Context, permission.Request, string) permission.Decision {
		return permission.Allowed()
	}},
		turn(exitCall("escape")),
		llmtest.Turn{Text: "child done"},
	)
	spawn := tool.New("spawn", "Runs a child.", func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
		for ev := range child.Run(ctx, agent.Input{Messages: []llm.Message{llm.UserText("go")}}) {
			if ev.Kind == agent.KindFinal {
				return tool.Text(string(ev.Final.Reason)), nil
			}
		}
		return tool.Errorf("no final"), nil
	}, tool.WithReadOnly())
	parent, _ := newAgent(t, Config{}, agent.Config{Tools: []tool.Tool{spawn}},
		turn(call("s", "spawn", obj{})),
		llmtest.Turn{Text: "done"},
	)
	runToEnd(t, parent)
	r := resultFor(t, childP, "x")
	if !r.IsError || !strings.Contains(text(r), "subagent") {
		t.Fatalf("child exit result = %+v", r)
	}
}

func TestPromptOverride(t *testing.T) {
	empty := ""
	a, p := newAgent(t, Config{Prompt: &empty}, agent.Config{PermissionMode: permission.ModePlan, System: []string{"host"}},
		llmtest.Turn{Text: "done"},
	)
	runToEnd(t, a)
	if len(p.Requests[0].System) != 1 {
		t.Fatalf("system = %q, want only the host segment", p.Requests[0].System)
	}
}

// recordingApprover records every request and answers with decide.
type recordingApprover struct {
	mu   sync.Mutex
	reqs []permission.Request
	next func(n int) permission.Decision
}

func (r *recordingApprover) approve(_ context.Context, req permission.Request, _ string) permission.Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return r.next(len(r.reqs))
}

func TestApproverRequestShowsPlanAndForbidsRemembering(t *testing.T) {
	// The largest plan exit_plan_mode accepts reaches the Approver whole.
	long := strings.Repeat("x", maxPlanBytes)
	rec := &recordingApprover{next: func(n int) permission.Decision {
		if n == 1 {
			return permission.Decision{Behavior: permission.Deny, Message: "revise"}
		}
		return permission.Allowed()
	}}
	a, _ := newAgent(t, Config{}, agent.Config{PermissionMode: permission.ModeAcceptEdits, Approver: rec.approve},
		turn(call("e", ToolEnter, obj{})),
		turn(exitCall("short plan")),
		turn(exitCall(long)),
		llmtest.Turn{Text: "done"},
	)
	runToEnd(t, a)
	if len(rec.reqs) != 2 {
		t.Fatalf("approver asked %d times, want 2 (an always-allow must not skip it)", len(rec.reqs))
	}
	for i, req := range rec.reqs {
		if !req.DisableRemember {
			t.Errorf("request %d: DisableRemember = false", i)
		}
	}
	if rec.reqs[0].Subject != "short plan" {
		t.Errorf("subject = %q, want the plan", rec.reqs[0].Subject)
	}
	if got := rec.reqs[1].Subject; got != long {
		t.Errorf("long subject len %d, want the whole plan of %d bytes", len(got), len(long))
	}
}

func TestPlanSubjectMarksCutPlan(t *testing.T) {
	raw, err := json.Marshal(ExitInput{Plan: strings.Repeat("x", maxSubjectBytes+1)})
	if err != nil {
		t.Fatal(err)
	}
	got := planSubject(raw)
	if len(got) != maxSubjectBytes+len(TruncationMarker) || !strings.HasSuffix(got, TruncationMarker) {
		t.Errorf("subject len %d, suffix ok = %v", len(got), strings.HasSuffix(got, TruncationMarker))
	}
}

func TestApproverUpdatedInputReplacesPlan(t *testing.T) {
	rec := &recordingApprover{next: func(int) permission.Decision {
		return permission.Decision{Behavior: permission.Allow, UpdatedInput: []byte(`{"plan":"edited plan"}`)}
	}}
	a, p := newAgent(t, Config{}, agent.Config{PermissionMode: permission.ModeAcceptEdits, Approver: rec.approve},
		turn(call("e", ToolEnter, obj{})),
		turn(exitCall("original")),
		llmtest.Turn{Text: "done"},
	)
	final := runToEnd(t, a)
	if plan, _ := Plan(final.State); plan != "edited plan" {
		t.Fatalf("planKey = %q, want the edited plan", plan)
	}
	if got := text(resultFor(t, p, "x")); !strings.Contains(got, "edited plan") {
		t.Fatalf("result = %q", got)
	}
}

func TestApproverUpdatedInputInvalidKeepsPlanMode(t *testing.T) {
	rec := &recordingApprover{next: func(int) permission.Decision {
		return permission.Decision{Behavior: permission.Allow, UpdatedInput: []byte(`{"plan":""}`)}
	}}
	a, p := newAgent(t, Config{}, agent.Config{PermissionMode: permission.ModeAcceptEdits, Approver: rec.approve},
		turn(call("e", ToolEnter, obj{})),
		turn(exitCall("original")),
		llmtest.Turn{Text: "done"},
	)
	final := runToEnd(t, a)
	if got := effectiveMode(a, final.State); got != permission.ModePlan {
		t.Fatalf("mode = %s, want plan", got)
	}
	if !resultFor(t, p, "x").IsError {
		t.Fatal("empty edited plan should be an error result")
	}
}

func TestEnterRefusedInSubagent(t *testing.T) {
	child, childP := newAgent(t, Config{}, agent.Config{PermissionMode: permission.ModeAcceptEdits},
		turn(call("e", ToolEnter, obj{})),
		llmtest.Turn{Text: "child done"},
	)
	spawn := tool.New("spawn", "Runs a child.", func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
		var last *agent.Final
		for ev := range child.Run(ctx, agent.Input{Messages: []llm.Message{llm.UserText("go")}}) {
			if ev.Kind == agent.KindFinal {
				last = ev.Final
			}
		}
		return tool.Text(string(effectiveMode(child, last.State))), nil
	}, tool.WithReadOnly())
	parent, parentP := newAgent(t, Config{}, agent.Config{Tools: []tool.Tool{spawn}},
		turn(call("s", "spawn", obj{})),
		llmtest.Turn{Text: "done"},
	)
	runToEnd(t, parent)
	r := resultFor(t, childP, "e")
	if r.IsError || !strings.Contains(text(r), "top-level") {
		t.Fatalf("child enter result = %+v", r)
	}
	if got := text(resultFor(t, parentP, "s")); got != string(permission.ModeAcceptEdits) {
		t.Fatalf("child mode = %q, want unchanged acceptEdits", got)
	}
}

func TestOversizePlanIsRefusedWithoutError(t *testing.T) {
	approver := func(context.Context, permission.Request, string) permission.Decision {
		t.Error("approver asked about an oversize plan")
		return permission.Allowed()
	}
	a, p := newAgent(t, Config{DisableEnter: true}, agent.Config{PermissionMode: permission.ModePlan, Approver: approver},
		turn(exitCall(strings.Repeat("x", maxPlanBytes+1))),
		llmtest.Turn{Text: "done"},
	)
	final := runToEnd(t, a)
	r := resultFor(t, p, "x")
	if r.IsError || !strings.Contains(text(r), "Shorten") {
		t.Fatalf("result = %+v", r)
	}
	if got := effectiveMode(a, final.State); got != permission.ModePlan {
		t.Fatalf("mode = %s", got)
	}
}

func TestWriteInSameTurnAsEnterIsDenied(t *testing.T) {
	w := &writes{}
	a, p := newAgent(t, Config{},
		agent.Config{PermissionMode: permission.ModeAcceptEdits, Tools: []tool.Tool{writeFile(w)}},
		turn(call("e", ToolEnter, obj{}), call("w", "write_file", obj{"path": "/a"})),
		llmtest.Turn{Text: "done"},
	)
	runToEnd(t, a)
	if got := text(resultFor(t, p, "w")); !strings.Contains(got, planDenied) {
		t.Fatalf("write result = %q, want %q", got, planDenied)
	}
	if w.count() != 0 {
		t.Fatal("write ran")
	}
}

func TestSiblingAfterPendingExitIsHeldThenReevaluated(t *testing.T) {
	dir := t.TempDir()
	w := &writes{}
	a, _ := newCheckpointed(t, dir, Config{}, permission.ModeAcceptEdits, w,
		turn(call("e", ToolEnter, obj{})),
		turn(exitCall("plan"), call("w", "write_file", obj{"path": "/a"})),
	)
	th, err := a.Thread(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if f := drain(t, th.Prompt(context.Background(), "go")); f.Reason != agent.ReasonInterrupted {
		t.Fatalf("reason = %s (%v)", f.Reason, f.Err)
	}
	if w.count() != 0 {
		t.Fatal("sibling ran while the exit was pending")
	}

	w2 := &writes{}
	a2, p2 := newCheckpointed(t, dir, Config{}, permission.ModeAcceptEdits, w2, llmtest.Turn{Text: "done"})
	th2, _ := pendingOf(t, a2, "t1")
	f := drain(t, th2.Resume(context.Background(), []agent.Decision{{ToolUseID: "x", Type: agent.Approve}}))
	if f.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v)", f.Reason, f.Err)
	}
	if w2.count() != 1 {
		t.Fatalf("sibling writes after approval = %d, want 1 (re-evaluated under the restored mode)", w2.count())
	}
	if r := resultFor(t, p2, "w"); r.IsError {
		t.Fatalf("sibling result = %+v", r)
	}
}

func TestExitRestoresDontAsk(t *testing.T) {
	approver := func(context.Context, permission.Request, string) permission.Decision { return permission.Allowed() }
	a, _ := newAgent(t, Config{}, agent.Config{PermissionMode: permission.ModeDontAsk, Approver: approver},
		turn(call("e", ToolEnter, obj{})),
		turn(exitCall("plan")),
		llmtest.Turn{Text: "done"},
	)
	final := runToEnd(t, a)
	if got := effectiveMode(a, final.State); got != permission.ModeDontAsk {
		t.Fatalf("mode = %s, want dontAsk", got)
	}
}

func TestPlanAccessor(t *testing.T) {
	if plan, ok := Plan(state.New()); ok || plan != "" {
		t.Fatalf("Plan on fresh state = %q, %v; want empty, false", plan, ok)
	}
	dir := t.TempDir()
	interruptedExit(t, dir, Config{}, permission.ModeAcceptEdits, "1. write a.txt")
	a, _ := newCheckpointed(t, dir, Config{}, permission.ModeAcceptEdits, &writes{}, llmtest.Turn{Text: "done"})
	th, intr := pendingOf(t, a, "t1")
	if intr == nil {
		t.Fatal("pending interrupt not restored")
	}
	final := drain(t, th.Resume(context.Background(), []agent.Decision{{ToolUseID: "x", Type: agent.Approve}}))
	if plan, ok := Plan(final.State); !ok || plan != "1. write a.txt" {
		t.Fatalf("Plan = %q, %v; want the approved plan, true", plan, ok)
	}
}

func TestName(t *testing.T) {
	if Name != "planmode" || mustNew(t, Config{}).Name() != Name {
		t.Fatalf("Name = %q", Name)
	}
}

func TestRequired(t *testing.T) {
	if !mustNew(t, Config{}).Required() {
		t.Fatal("Required() = false, want true")
	}
}
