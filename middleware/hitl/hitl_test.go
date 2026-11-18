package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

type obj = map[string]any

type pathInput struct {
	Path string `json:"path"`
}

type commandInput struct {
	Command string `json:"command"`
}

// log records what the tools received, in order.
type log struct {
	mu      sync.Mutex
	entries []string
	resumes []*tool.ResumeValue
}

func (l *log) add(entry string, c *tool.Call) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	l.resumes = append(l.resumes, c.Resume)
}

func (l *log) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

func writeFile(l *log) tool.Tool {
	return tool.New("write_file", "Write a file.",
		func(_ context.Context, in pathInput, c *tool.Call) (tool.Result, error) {
			l.add(in.Path, c)
			return tool.Text("wrote " + in.Path), nil
		},
		tool.WithEdit(),
		tool.WithPaths(func(in json.RawMessage) []permission.PathAccess {
			var p pathInput
			_ = json.Unmarshal(in, &p)
			return []permission.PathAccess{{Path: p.Path, Op: permission.OpWrite}}
		}))
}

func execute(l *log) tool.Tool {
	return tool.New("execute", "Run a command.",
		func(_ context.Context, in commandInput, c *tool.Call) (tool.Result, error) {
			l.add(in.Command, c)
			return tool.Text("ran " + in.Command), nil
		})
}

// selfInterrupting raises its own interrupt the first time it runs and
// completes once resumed.
func selfInterrupting(l *log) tool.Tool {
	return tool.New("deploy", "Deploy.",
		func(_ context.Context, in pathInput, c *tool.Call) (tool.Result, error) {
			l.add(in.Path, c)
			if c.Resume == nil {
				return tool.Result{}, &agent.Interrupt{Requests: []agent.ActionRequest{{Reason: "tool asks"}}}
			}
			return tool.Text("deployed " + in.Path), nil
		})
}

func call(id, name string, input any) llmtest.Call {
	return llmtest.Call{ID: id, Name: name, Input: input}
}

func newAgent(t *testing.T, cfg Config, ac agent.Config, turns ...llmtest.Turn) *agent.Agent {
	t.Helper()
	ac.Provider = llmtest.New(turns...)
	ac.Middleware = append(ac.Middleware, New(cfg))
	a, err := agent.New(ac)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func drain(t *testing.T, seq func(func(agent.Event, error) bool)) ([]agent.Event, *agent.Final) {
	t.Helper()
	var events []agent.Event
	var final *agent.Final
	for ev := range seq {
		events = append(events, ev)
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatal("no final event")
	}
	return events, final
}

func run(t *testing.T, a *agent.Agent) ([]agent.Event, *agent.Final) {
	t.Helper()
	return drain(t, a.Run(context.Background(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}))
}

func resume(t *testing.T, a *agent.Agent, s *state.State, d ...agent.Decision) ([]agent.Event, *agent.Final) {
	t.Helper()
	return drain(t, a.Resume(context.Background(), "", s, d))
}

func mustInterrupt(t *testing.T, events []agent.Event, f *agent.Final) *agent.Interrupt {
	t.Helper()
	if f.Reason != agent.ReasonInterrupted {
		t.Fatalf("final = %v / %v, want interrupted", f.Reason, f.Err)
	}
	for _, ev := range events {
		if ev.Kind == agent.KindInterrupt {
			return ev.Interrupt
		}
	}
	t.Fatal("no KindInterrupt event")
	return nil
}

func mustComplete(t *testing.T, f *agent.Final) {
	t.Helper()
	if f.Reason != agent.ReasonCompleted || f.Err != nil {
		t.Fatalf("final = %v / %v, want completed", f.Reason, f.Err)
	}
}

func hasInterrupt(events []agent.Event) bool {
	return slices.ContainsFunc(events, func(e agent.Event) bool { return e.Kind == agent.KindInterrupt })
}

// result returns the text and error flag of the tool_result for id.
func result(t *testing.T, s *state.State, id string) (string, bool) {
	t.Helper()
	for _, m := range s.Messages() {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && b.ToolUseID == id {
				var parts []string
				for _, c := range b.Content {
					parts = append(parts, c.Text)
				}
				return strings.Join(parts, "\n"), b.IsError
			}
		}
	}
	t.Fatalf("no tool_result for %q", id)
	return "", false
}

func on(tools ...string) Config {
	cfg := Config{InterruptOn: map[string]InterruptConfig{}}
	for _, n := range tools {
		cfg.InterruptOn[n] = InterruptConfig{}
	}
	return cfg
}

var bypass = agent.Config{PermissionMode: permission.ModeBypass}

func withTools(ac agent.Config, tools ...tool.Tool) agent.Config {
	ac.Tools = tools
	return ac
}

func writeTurns(path string) []llmtest.Turn {
	return []llmtest.Turn{
		{ToolCalls: []llmtest.Call{call("w", "write_file", obj{"path": path})}},
		{Text: "done"},
	}
}

func TestName(t *testing.T) {
	if got := New(Config{}).Name(); got != "hitl" {
		t.Fatalf("Name() = %q, want hitl", got)
	}
}

func TestInterruptOnPausesEvenInBypass(t *testing.T) {
	l := &log{}
	a := newAgent(t, on("write_file"), withTools(bypass, writeFile(l)), writeTurns("/a.txt")...)
	events, final := run(t, a)
	intr := mustInterrupt(t, events, final)

	if len(intr.Requests) != 1 {
		t.Fatalf("requests = %+v, want one", intr.Requests)
	}
	req := intr.Requests[0]
	if req.ToolUseID != "w" || req.Tool != "write_file" {
		t.Fatalf("request = %+v, want write_file call w", req)
	}
	if !slices.Equal(req.Allowed, DefaultAllowed) {
		t.Fatalf("Allowed = %v, want %v", req.Allowed, DefaultAllowed)
	}
	if req.Reason != "Approval required for write_file" {
		t.Fatalf("Reason = %q, want the default reason", req.Reason)
	}
	if len(l.all()) != 0 {
		t.Fatalf("tool ran %v before approval", l.all())
	}
}

func TestCustomReasonAndAllowed(t *testing.T) {
	cfg := Config{InterruptOn: map[string]InterruptConfig{
		"write_file": {Allowed: []agent.DecisionType{agent.Reject}, Reason: "sensitive"},
	}}
	a := newAgent(t, cfg, withTools(bypass, writeFile(&log{})), writeTurns("/a.txt")...)
	events, final := run(t, a)
	req := mustInterrupt(t, events, final).Requests[0]
	if req.Reason != "sensitive" || !slices.Equal(req.Allowed, []agent.DecisionType{agent.Reject}) {
		t.Fatalf("request = %+v, want the configured reason and allowed", req)
	}
}

func TestUnconfiguredToolRuns(t *testing.T) {
	l := &log{}
	a := newAgent(t, on("execute"), withTools(bypass, writeFile(l)), writeTurns("/a.txt")...)
	_, final := run(t, a)
	mustComplete(t, final)
	if !slices.Equal(l.all(), []string{"/a.txt"}) {
		t.Fatalf("ran %v, want the write", l.all())
	}
}

func TestResumeApproveRunsOnce(t *testing.T) {
	l := &log{}
	a := newAgent(t, on("write_file"), withTools(bypass, writeFile(l)), writeTurns("/a.txt")...)
	events, final := run(t, a)
	mustInterrupt(t, events, final)

	_, final = resume(t, a, final.State, agent.Decision{ToolUseID: "w", Type: agent.Approve})
	mustComplete(t, final)
	if !slices.Equal(l.all(), []string{"/a.txt"}) {
		t.Fatalf("ran %v, want exactly one run", l.all())
	}
	if l.resumes[0] != nil {
		t.Fatalf("tool saw Resume %+v, want it cleared", l.resumes[0])
	}
	if text, isErr := result(t, final.State, "w"); isErr || text != "wrote /a.txt" {
		t.Fatalf("result = %q (error %v), want the tool output", text, isErr)
	}
}

func TestResumeEditUsesNewInput(t *testing.T) {
	l := &log{}
	a := newAgent(t, on("write_file"), withTools(bypass, writeFile(l)), writeTurns("/a.txt")...)
	events, final := run(t, a)
	mustInterrupt(t, events, final)

	_, final = resume(t, a, final.State, agent.Decision{ToolUseID: "w", Type: agent.Edit, Input: json.RawMessage(`{"path":"/b.txt"}`)})
	mustComplete(t, final)
	if !slices.Equal(l.all(), []string{"/b.txt"}) {
		t.Fatalf("ran %v, want the edited input", l.all())
	}
}

func TestResumeEditToDeniedPathIsRefused(t *testing.T) {
	l := &log{}
	ac := withTools(bypass, writeFile(l))
	ac.Rules = permission.Rules{Deny: []permission.Rule{{Op: permission.OpWrite, Pattern: "/secret/*"}}}
	a := newAgent(t, on("write_file"), ac, writeTurns("/a.txt")...)
	events, final := run(t, a)
	mustInterrupt(t, events, final)

	_, final = resume(t, a, final.State, agent.Decision{ToolUseID: "w", Type: agent.Edit, Input: json.RawMessage(`{"path":"/secret/key"}`)})
	mustComplete(t, final)
	if len(l.all()) != 0 {
		t.Fatalf("tool ran %v, want the denied edit refused", l.all())
	}
	if text, isErr := result(t, final.State, "w"); !isErr || !strings.HasPrefix(text, "Permission denied") {
		t.Fatalf("result = %q (error %v), want Permission denied", text, isErr)
	}
}

func TestResumeRejectNotRun(t *testing.T) {
	l := &log{}
	a := newAgent(t, on("write_file"), withTools(bypass, writeFile(l)), writeTurns("/a.txt")...)
	events, final := run(t, a)
	mustInterrupt(t, events, final)

	_, final = resume(t, a, final.State, agent.Decision{ToolUseID: "w", Type: agent.Reject, Message: "no way"})
	mustComplete(t, final)
	if len(l.all()) != 0 {
		t.Fatalf("tool ran %v, want it never run", l.all())
	}
	if text, isErr := result(t, final.State, "w"); !isErr || !strings.Contains(text, "no way") {
		t.Fatalf("result = %q (error %v), want the rejection message", text, isErr)
	}
}

func TestRespondReturnsMessage(t *testing.T) {
	l := &log{}
	cfg := Config{InterruptOn: map[string]InterruptConfig{
		"write_file": {Allowed: []agent.DecisionType{agent.Approve, agent.Respond}},
	}}
	a := newAgent(t, cfg, withTools(bypass, writeFile(l)), writeTurns("/a.txt")...)
	events, final := run(t, a)
	mustInterrupt(t, events, final)

	_, final = resume(t, a, final.State, agent.Decision{ToolUseID: "w", Type: agent.Respond, Message: "use /tmp instead"})
	mustComplete(t, final)
	if len(l.all()) != 0 {
		t.Fatalf("tool ran %v, want it answered without running", l.all())
	}
	if text, isErr := result(t, final.State, "w"); isErr || text != "use /tmp instead" {
		t.Fatalf("result = %q (error %v), want the message as a success", text, isErr)
	}
}

func TestWhenPredicate(t *testing.T) {
	l := &log{}
	cfg := Config{InterruptOn: map[string]InterruptConfig{
		"execute": {When: func(_ context.Context, c *agent.ToolCall) bool {
			var in commandInput
			_ = json.Unmarshal(c.Input, &in)
			return strings.HasPrefix(in.Command, "rm")
		}},
	}}
	a := newAgent(t, cfg, withTools(bypass, execute(l)),
		llmtest.Turn{ToolCalls: []llmtest.Call{
			call("safe", "execute", obj{"command": "ls"}),
			call("risky", "execute", obj{"command": "rm -rf x"}),
		}},
		llmtest.Turn{Text: "done"})
	events, final := run(t, a)
	intr := mustInterrupt(t, events, final)

	if len(intr.Requests) != 1 || intr.Requests[0].ToolUseID != "risky" {
		t.Fatalf("requests = %+v, want only the rm command", intr.Requests)
	}
	if !slices.Equal(l.all(), []string{"ls"}) {
		t.Fatalf("ran %v, want only ls", l.all())
	}
}

func TestApproverUsedSynchronously(t *testing.T) {
	l := &log{}
	var asked atomic.Int32
	var seen permission.Request
	var reason string
	ac := withTools(bypass, writeFile(l))
	ac.Approver = func(_ context.Context, req permission.Request, r string) permission.Decision {
		asked.Add(1)
		seen, reason = req, r
		return permission.Denied("not today")
	}
	a := newAgent(t, on("write_file"), ac, writeTurns("/a.txt")...)
	events, final := run(t, a)

	mustComplete(t, final)
	if hasInterrupt(events) {
		t.Fatal("run interrupted, want the Approver asked synchronously")
	}
	if asked.Load() != 1 || seen.Tool != "write_file" || reason != "Approval required for write_file" {
		t.Fatalf("approver asked %d times with %q / %q, want once about write_file", asked.Load(), seen.Tool, reason)
	}
	if len(l.all()) != 0 {
		t.Fatalf("tool ran %v, want it refused", l.all())
	}
	if text, isErr := result(t, final.State, "w"); !isErr || text != "Rejected: not today" {
		t.Fatalf("result = %q (error %v), want Rejected: not today", text, isErr)
	}
}

func TestApproverAllowRunsWithUpdatedInput(t *testing.T) {
	l := &log{}
	ac := withTools(bypass, writeFile(l))
	ac.Approver = func(context.Context, permission.Request, string) permission.Decision {
		return permission.Decision{Behavior: permission.Allow, UpdatedInput: json.RawMessage(`{"path":"/safe.txt"}`)}
	}
	a := newAgent(t, on("write_file"), ac, writeTurns("/a.txt")...)
	events, final := run(t, a)
	mustComplete(t, final)
	if hasInterrupt(events) || !slices.Equal(l.all(), []string{"/safe.txt"}) {
		t.Fatalf("ran %v, want the approver's rewritten input", l.all())
	}
}

func TestNoDoublePromptAfterPermissionAsk(t *testing.T) {
	l := &log{}
	var asked atomic.Int32
	ac := withTools(agent.Config{}, writeFile(l))
	ac.Rules = permission.Rules{Ask: []permission.Rule{{Tool: "write_file"}}}
	ac.Approver = func(context.Context, permission.Request, string) permission.Decision {
		asked.Add(1)
		return permission.Allowed()
	}
	a := newAgent(t, on("write_file"), ac, writeTurns("/a.txt")...)
	events, final := run(t, a)

	mustComplete(t, final)
	if hasInterrupt(events) || asked.Load() != 1 {
		t.Fatalf("asked %d times, want the permission step's single prompt", asked.Load())
	}
	if !slices.Equal(l.all(), []string{"/a.txt"}) {
		t.Fatalf("ran %v, want the approved write", l.all())
	}
}

func TestToolOwnInterruptAfterHitl(t *testing.T) {
	l := &log{}
	a := newAgent(t, on("deploy"), withTools(bypass, selfInterrupting(l)),
		llmtest.Turn{ToolCalls: []llmtest.Call{call("d", "deploy", obj{"path": "/prod"})}},
		llmtest.Turn{Text: "done"})
	events, final := run(t, a)
	if got := mustInterrupt(t, events, final).Requests[0].Reason; got != "Approval required for deploy" {
		t.Fatalf("first interrupt reason = %q, want hitl's", got)
	}

	events, final = resume(t, a, final.State, agent.Decision{ToolUseID: "d", Type: agent.Approve})
	if got := mustInterrupt(t, events, final).Requests[0].Reason; got != "tool asks" {
		t.Fatalf("second interrupt reason = %q, want the tool's own", got)
	}

	_, final = resume(t, a, final.State, agent.Decision{ToolUseID: "d", Type: agent.Approve})
	mustComplete(t, final)
	if len(l.resumes) != 2 || l.resumes[0] != nil || l.resumes[1] == nil || l.resumes[1].Type != tool.ResumeApprove {
		t.Fatalf("tool resumes = %+v, want [nil, approve]", l.resumes)
	}
	if text, _ := result(t, final.State, "d"); text != "deployed /prod" {
		t.Fatalf("result = %q, want the tool to complete", text)
	}
}

func TestWhenFlippingFalseAfterEditStillClearsResume(t *testing.T) {
	l := &log{}
	cfg := Config{InterruptOn: map[string]InterruptConfig{
		"execute": {When: func(_ context.Context, c *agent.ToolCall) bool {
			var in commandInput
			_ = json.Unmarshal(c.Input, &in)
			return strings.HasPrefix(in.Command, "rm")
		}},
	}}
	a := newAgent(t, cfg, withTools(bypass, execute(l)),
		llmtest.Turn{ToolCalls: []llmtest.Call{call("x", "execute", obj{"command": "rm x"})}},
		llmtest.Turn{Text: "done"})
	events, final := run(t, a)
	mustInterrupt(t, events, final)

	_, final = resume(t, a, final.State, agent.Decision{ToolUseID: "x", Type: agent.Edit, Input: json.RawMessage(`{"command":"ls"}`)})
	mustComplete(t, final)
	if !slices.Equal(l.all(), []string{"ls"}) {
		t.Fatalf("ran %v, want the edited command", l.all())
	}
	if l.resumes[0] != nil {
		t.Fatalf("tool saw Resume %+v, want hitl's answer consumed", l.resumes[0])
	}
}

func TestTwoConfiguredCallsInterruptAndResume(t *testing.T) {
	l := &log{}
	a := newAgent(t, on("write_file"), withTools(bypass, writeFile(l)),
		llmtest.Turn{ToolCalls: []llmtest.Call{
			call("w1", "write_file", obj{"path": "/1"}),
			call("w2", "write_file", obj{"path": "/2"}),
		}},
		llmtest.Turn{Text: "done"})
	// A tool-raised interrupt halts the turn: the second call waits for its
	// own turn at the human once the first is decided.
	events, final := run(t, a)
	if id := mustInterrupt(t, events, final).Requests[0].ToolUseID; id != "w1" {
		t.Fatalf("first interrupt is for %q, want w1", id)
	}
	events, final = resume(t, a, final.State, agent.Decision{ToolUseID: "w1", Type: agent.Approve})
	if id := mustInterrupt(t, events, final).Requests[0].ToolUseID; id != "w2" {
		t.Fatalf("second interrupt is for %q, want w2", id)
	}
	_, final = resume(t, a, final.State, agent.Decision{ToolUseID: "w2", Type: agent.Approve})
	mustComplete(t, final)
	if !slices.Equal(l.all(), []string{"/1", "/2"}) {
		t.Fatalf("ran %v, want both writes once, in order", l.all())
	}
}

func TestRespondOutsideAllowedIsInvalid(t *testing.T) {
	a := newAgent(t, on("write_file"), withTools(bypass, writeFile(&log{})), writeTurns("/a.txt")...)
	events, final := run(t, a)
	mustInterrupt(t, events, final)

	_, final = resume(t, a, final.State, agent.Decision{ToolUseID: "w", Type: agent.Respond, Message: "hi"})
	if final.Reason != agent.ReasonInvalidResume || !errors.Is(final.Err, agent.ErrInvalidDecision) {
		t.Fatalf("final = %v / %v, want invalid resume with ErrInvalidDecision", final.Reason, final.Err)
	}
}

func TestApproverRewriteToDeniedPathIsRefused(t *testing.T) {
	l := &log{}
	ac := withTools(bypass, writeFile(l))
	ac.Rules = permission.Rules{Deny: []permission.Rule{{Op: permission.OpWrite, Pattern: "/secret/*"}}}
	ac.Approver = func(context.Context, permission.Request, string) permission.Decision {
		return permission.Decision{Behavior: permission.Allow, UpdatedInput: json.RawMessage(`{"path":"/secret/key"}`)}
	}
	a := newAgent(t, on("write_file"), ac, writeTurns("/a.txt")...)
	_, final := run(t, a)
	mustComplete(t, final)
	if len(l.all()) != 0 {
		t.Fatalf("ran %v, want the denied rewrite refused", l.all())
	}
	if text, isErr := result(t, final.State, "w"); !isErr || !strings.HasPrefix(text, "Permission denied") {
		t.Fatalf("result = %q (error %v), want Permission denied", text, isErr)
	}
}

func TestApproverRewriteIgnoredWhenEditNotAllowed(t *testing.T) {
	l := &log{}
	cfg := Config{InterruptOn: map[string]InterruptConfig{
		"write_file": {Allowed: []agent.DecisionType{agent.Approve, agent.Reject}},
	}}
	ac := withTools(bypass, writeFile(l))
	ac.Approver = func(context.Context, permission.Request, string) permission.Decision {
		return permission.Decision{Behavior: permission.Allow, UpdatedInput: json.RawMessage(`{"path":"/other"}`)}
	}
	a := newAgent(t, cfg, ac, writeTurns("/a.txt")...)
	_, final := run(t, a)
	mustComplete(t, final)
	if !slices.Equal(l.all(), []string{"/a.txt"}) {
		t.Fatalf("ran %v, want the original input", l.all())
	}
}

func TestRejectedWithoutMessage(t *testing.T) {
	ac := withTools(bypass, writeFile(&log{}))
	ac.Approver = func(context.Context, permission.Request, string) permission.Decision {
		return permission.Decision{Behavior: permission.Deny}
	}
	a := newAgent(t, on("write_file"), ac, writeTurns("/a.txt")...)
	_, final := run(t, a)
	if text, isErr := result(t, final.State, "w"); !isErr || text != "Rejected." {
		t.Fatalf("result = %q (error %v), want Rejected.", text, isErr)
	}
}
