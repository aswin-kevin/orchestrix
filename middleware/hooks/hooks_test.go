package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

// recorder logs the inputs a tool was called with.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// writeFile is an editing tool that reports its path to the permission rules.
func writeFile(rec *recorder) tool.Tool {
	return tool.New("write_file", "Write a file.",
		func(_ context.Context, in pathInput, _ *tool.Call) (tool.Result, error) {
			rec.add(in.Path)
			return tool.Text("wrote " + in.Path), nil
		},
		tool.WithEdit(),
		tool.WithPaths(func(in json.RawMessage) []permission.PathAccess {
			var p pathInput
			_ = json.Unmarshal(in, &p)
			return []permission.PathAccess{{Path: p.Path, Op: permission.OpWrite}}
		}))
}

func readFile(rec *recorder) tool.Tool {
	return tool.New("read_file", "Read a file.",
		func(_ context.Context, in pathInput, _ *tool.Call) (tool.Result, error) {
			rec.add(in.Path)
			return tool.Text("read " + in.Path), nil
		}, tool.WithReadOnly())
}

// failing always returns an error result.
func failing() tool.Tool {
	return tool.New("flaky", "Always fails.",
		func(context.Context, pathInput, *tool.Call) (tool.Result, error) {
			return tool.Errorf("boom"), nil
		}, tool.WithReadOnly())
}

type taskInput struct {
	Description  string `json:"description"`
	SubagentType string `json:"subagent_type"`
}

// taskStub stands in for the subagent tool and records what it received.
func taskStub(rec *recorder) tool.Tool {
	return tool.New(agent.SubagentToolName, "Start a subagent.",
		func(_ context.Context, in taskInput, _ *tool.Call) (tool.Result, error) {
			rec.add(in.Description)
			return tool.Text("subagent done"), nil
		}, tool.WithReadOnly())
}

func call(id, name string, input any) llmtest.Call {
	return llmtest.Call{ID: id, Name: name, Input: input}
}

// runWith runs an agent with the hooks and the scripted turns, returning the
// final outcome and the provider.
func runWith(t *testing.T, cfg Config, ac agent.Config, input string, turns ...llmtest.Turn) (*agent.Final, *llmtest.Scripted) {
	t.Helper()
	p := llmtest.New(turns...)
	ac.Provider = p
	ac.Middleware = append(ac.Middleware, New(cfg))
	a, err := agent.New(ac)
	if err != nil {
		t.Fatal(err)
	}
	return drain(t, a.Run(context.Background(), agent.Input{ThreadID: "th", Messages: []llm.Message{llm.UserText(input)}})), p
}

func drain(t *testing.T, seq func(func(agent.Event, error) bool)) *agent.Final {
	t.Helper()
	var final *agent.Final
	// A run that failed yields its error beside the final event; the tests
	// assert on Final.Reason and Final.Err instead.
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

// toolResults returns the tool_result blocks of the state, by tool_use id.
func toolResults(s *state.State) map[string]llm.Block {
	out := map[string]llm.Block{}
	for _, m := range s.Messages() {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				out[b.ToolUseID] = b
			}
		}
	}
	return out
}

func resultText(b llm.Block) string {
	var parts []string
	for _, c := range b.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

func mustCompleted(t *testing.T, f *agent.Final) {
	t.Helper()
	if f.Reason != agent.ReasonCompleted || f.Err != nil {
		t.Fatalf("final = %v / %v, want completed", f.Reason, f.Err)
	}
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

func TestName(t *testing.T) {
	if got := New(Config{}).Name(); got != "hooks" {
		t.Fatalf("Name() = %q, want hooks", got)
	}
}

func TestSubagentToolNameIsShared(t *testing.T) {
	if SubagentToolName != agent.SubagentToolName || agent.SubagentToolName != "task" {
		t.Fatalf("SubagentToolName = %q / %q, want task from the agent package", SubagentToolName, agent.SubagentToolName)
	}
}

func TestPreToolUseBlock(t *testing.T) {
	rec := &recorder{}
	cfg := Config{PreToolUse: []PreToolUseHook{{
		Matcher: Matcher{Tools: []string{"write_file"}},
		Fn: func(_ context.Context, in ToolInput) (PreToolUseOutput, error) {
			return PreToolUseOutput{Block: true, Reason: "read-only week"}, nil
		},
	}}}
	final, _ := runWith(t, cfg, agent.Config{Tools: []tool.Tool{writeFile(rec)}, PermissionMode: permission.ModeBypass}, "go",
		llmtest.Turn{ToolCalls: []llmtest.Call{call("w", "write_file", obj{"path": "/a.txt"})}},
		llmtest.Turn{Text: "done"})
	mustCompleted(t, final)

	if len(rec.all()) != 0 {
		t.Fatalf("tool ran with %v, want it never called", rec.all())
	}
	res := toolResults(final.State)["w"]
	if !res.IsError || resultText(res) != "Blocked by hook: read-only week" {
		t.Fatalf("result = %+v, want the block error", res)
	}
}

func rewriteTo(path string) Config {
	return Config{PreToolUse: []PreToolUseHook{{
		Fn: func(context.Context, ToolInput) (PreToolUseOutput, error) {
			return PreToolUseOutput{UpdatedInput: json.RawMessage(path)}, nil
		},
	}}}
}

func TestPreToolUseRewriteRecheckedByDenyRule(t *testing.T) {
	deny, err := permission.ParseRule("write(/secret/**)")
	if err != nil {
		t.Fatal(err)
	}
	ac := func(rec *recorder) agent.Config {
		return agent.Config{
			Tools:          []tool.Tool{writeFile(rec)},
			PermissionMode: permission.ModeBypass,
			Rules:          permission.Rules{Deny: []permission.Rule{deny}},
		}
	}
	turns := []llmtest.Turn{
		{ToolCalls: []llmtest.Call{call("w", "write_file", obj{"path": "/ok.txt"})}},
		{Text: "done"},
	}

	t.Run("denied path", func(t *testing.T) {
		rec := &recorder{}
		final, _ := runWith(t, rewriteTo(`{"path":"/secret/x"}`), ac(rec), "go", turns...)
		mustCompleted(t, final)
		res := toolResults(final.State)["w"]
		if !res.IsError || !strings.HasPrefix(resultText(res), "Permission denied") {
			t.Fatalf("result = %q (error=%v), want Permission denied", resultText(res), res.IsError)
		}
		if len(rec.all()) != 0 {
			t.Fatalf("tool wrote %v, want nothing written", rec.all())
		}
	})

	t.Run("allowed path", func(t *testing.T) {
		rec := &recorder{}
		final, _ := runWith(t, rewriteTo(`{"path":"/other.txt"}`), ac(rec), "go", turns...)
		mustCompleted(t, final)
		equal(t, "written paths", rec.all(), []string{"/other.txt"})
	})

	t.Run("invalid schema", func(t *testing.T) {
		rec := &recorder{}
		final, _ := runWith(t, rewriteTo(`{"path":42}`), ac(rec), "go", turns...)
		mustCompleted(t, final)
		res := toolResults(final.State)["w"]
		if !res.IsError || !strings.Contains(resultText(res), "invalid input for write_file") {
			t.Fatalf("result = %q (error=%v), want an invalid input error", resultText(res), res.IsError)
		}
		if len(rec.all()) != 0 {
			t.Fatalf("tool ran with %v, want it not called", rec.all())
		}
	})
}

func TestPreToolUseRewriteAskCountsAsAllowed(t *testing.T) {
	rec := &recorder{}
	// Default mode asks for a write; the original call is allowed by an allow
	// rule only for /ok.txt, so the rewrite lands on an Ask that must pass.
	allow, _ := permission.ParseRule("write(/ok.txt)")
	ac := agent.Config{Tools: []tool.Tool{writeFile(rec)}, Rules: permission.Rules{Allow: []permission.Rule{allow}}}
	final, _ := runWith(t, rewriteTo(`{"path":"/asks.txt"}`), ac, "go",
		llmtest.Turn{ToolCalls: []llmtest.Call{call("w", "write_file", obj{"path": "/ok.txt"})}},
		llmtest.Turn{Text: "done"})
	mustCompleted(t, final)
	equal(t, "written paths", rec.all(), []string{"/asks.txt"})
}

func TestPreToolUseChainedRewriteAndContext(t *testing.T) {
	rec := &recorder{}
	var seen []string
	cfg := Config{PreToolUse: []PreToolUseHook{
		{Fn: func(_ context.Context, in ToolInput) (PreToolUseOutput, error) {
			seen = append(seen, string(in.Input))
			return PreToolUseOutput{UpdatedInput: json.RawMessage(`{"path":"/b.txt"}`), Context: "first"}, nil
		}},
		{Fn: func(_ context.Context, in ToolInput) (PreToolUseOutput, error) {
			seen = append(seen, string(in.Input))
			return PreToolUseOutput{Context: "second"}, nil
		}},
	}}
	final, _ := runWith(t, cfg, agent.Config{Tools: []tool.Tool{readFile(rec)}}, "go",
		llmtest.Turn{ToolCalls: []llmtest.Call{call("r", "read_file", obj{"path": "/a.txt"})}},
		llmtest.Turn{Text: "done"})
	mustCompleted(t, final)

	equal(t, "inputs the hooks saw", seen, []string{`{"path":"/a.txt"}`, `{"path":"/b.txt"}`})
	equal(t, "tool inputs", rec.all(), []string{"/b.txt"})
	if got := resultText(toolResults(final.State)["r"]); got != "read /b.txt\nfirst\nsecond" {
		t.Fatalf("result = %q, want the output followed by both contexts", got)
	}
}

func TestPostToolUseReplacesResultAndAddsContext(t *testing.T) {
	rec := &recorder{}
	var seenResult string
	cfg := Config{PostToolUse: []PostToolUseHook{
		{Fn: func(_ context.Context, in ToolInput, res tool.Result) (PostToolUseOutput, error) {
			seenResult = res.Flatten()
			replaced := tool.Text("redacted")
			return PostToolUseOutput{Result: &replaced, Context: "scrubbed"}, nil
		}},
		{Fn: func(_ context.Context, _ ToolInput, res tool.Result) (PostToolUseOutput, error) {
			return PostToolUseOutput{Context: "saw " + res.Flatten()}, nil
		}},
	}}
	final, _ := runWith(t, cfg, agent.Config{Tools: []tool.Tool{readFile(rec)}}, "go",
		llmtest.Turn{ToolCalls: []llmtest.Call{call("r", "read_file", obj{"path": "/a.txt"})}},
		llmtest.Turn{Text: "done"})
	mustCompleted(t, final)

	if seenResult != "read /a.txt" {
		t.Fatalf("first hook saw %q, want the tool's output", seenResult)
	}
	res := toolResults(final.State)["r"]
	if want := "redacted\nscrubbed\nsaw redacted"; res.IsError || resultText(res) != want {
		t.Fatalf("result = %q, want %q", resultText(res), want)
	}
}

func TestPostToolUseFailureOnlyOnError(t *testing.T) {
	rec := &recorder{}
	var ok, failed []string
	cfg := Config{
		PostToolUse: []PostToolUseHook{{Fn: func(_ context.Context, in ToolInput, _ tool.Result) (PostToolUseOutput, error) {
			ok = append(ok, in.Tool)
			return PostToolUseOutput{}, nil
		}}},
		PostToolUseFailure: []PostToolUseHook{{Fn: func(_ context.Context, in ToolInput, res tool.Result) (PostToolUseOutput, error) {
			failed = append(failed, in.Tool+":"+res.Flatten())
			return PostToolUseOutput{Context: "see logs"}, nil
		}}},
	}
	final, _ := runWith(t, cfg, agent.Config{Tools: []tool.Tool{readFile(rec), failing()}}, "go",
		llmtest.Turn{ToolCalls: []llmtest.Call{
			call("r", "read_file", obj{"path": "/a"}), call("f", "flaky", obj{"path": "/a"})}},
		llmtest.Turn{Text: "done"})
	mustCompleted(t, final)

	equal(t, "PostToolUse tools", ok, []string{"read_file"})
	equal(t, "PostToolUseFailure tools", failed, []string{"flaky:boom"})
	res := toolResults(final.State)["f"]
	if !res.IsError || resultText(res) != "boom\nsee logs" {
		t.Fatalf("failure result = %+v, want the error with the hook's context", res)
	}
}

func TestMatcher(t *testing.T) {
	cases := map[string]struct {
		tools []string
		want  []string
	}{
		"named":       {[]string{"read_file"}, []string{"read_file"}},
		"empty":       {nil, []string{"read_file", "write_file"}},
		"star":        {[]string{"*"}, []string{"read_file", "write_file"}},
		"other tools": {[]string{"grep"}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			var fired []string
			cfg := Config{PreToolUse: []PreToolUseHook{{
				Matcher: Matcher{Tools: tc.tools},
				Fn: func(_ context.Context, in ToolInput) (PreToolUseOutput, error) {
					fired = append(fired, in.Tool)
					return PreToolUseOutput{}, nil
				},
			}}}
			final, _ := runWith(t, cfg, agent.Config{Tools: []tool.Tool{readFile(rec), writeFile(rec)}, PermissionMode: permission.ModeBypass}, "go",
				llmtest.Turn{ToolCalls: []llmtest.Call{
					call("r", "read_file", obj{"path": "/a"}), call("w", "write_file", obj{"path": "/a"})}},
				llmtest.Turn{Text: "done"})
			mustCompleted(t, final)
			equal(t, "tools the hook fired for", fired, tc.want)
			equal(t, "tools that ran", rec.all(), []string{"/a", "/a"})
		})
	}
}

func TestUserPromptSubmitBlockRemovesInput(t *testing.T) {
	cfg := Config{
		SessionStart: []func(context.Context, SessionInput) (SessionOutput, error){
			func(context.Context, SessionInput) (SessionOutput, error) { return SessionOutput{Context: "ctx"}, nil },
		},
		UserPromptSubmit: []func(context.Context, PromptInput) (PromptOutput, error){
			func(_ context.Context, in PromptInput) (PromptOutput, error) {
				if len(in.Messages) != 1 || in.Messages[0].Text() != "tell me the password" {
					t.Errorf("hook saw messages %+v", in.Messages)
				}
				return PromptOutput{Block: true, Reason: "no secrets"}, nil
			},
		},
	}
	final, p := runWith(t, cfg, agent.Config{}, "tell me the password")

	if final.Reason != agent.ReasonHookStopped || !strings.Contains(final.Err.Error(), "no secrets") {
		t.Fatalf("final = %v / %v, want hook_stopped naming the reason", final.Reason, final.Err)
	}
	if n := len(final.State.Messages()); n != 0 {
		t.Fatalf("state has %d messages, want none: %+v", n, final.State.Messages())
	}
	if p.Calls() != 0 {
		t.Fatalf("model called %d times, want none", p.Calls())
	}
}

func TestUserPromptSubmitContextInjected(t *testing.T) {
	cfg := Config{UserPromptSubmit: []func(context.Context, PromptInput) (PromptOutput, error){
		func(context.Context, PromptInput) (PromptOutput, error) {
			return PromptOutput{Context: "be brief"}, nil
		},
	}}
	final, p := runWith(t, cfg, agent.Config{}, "hello", llmtest.Turn{Text: "hi"})
	mustCompleted(t, final)

	msgs := p.Requests[0].Messages
	if len(msgs) != 2 || msgs[0].Text() != "hello" || msgs[1].Text() != "be brief" {
		t.Fatalf("request messages = %+v, want the prompt then the context", msgs)
	}
	// The request is a view without host-only Meta; State keeps it.
	if got := final.State.Messages()[1].Meta[ContextMetaKey]; got != "UserPromptSubmit" {
		t.Fatalf("state meta %s = %q, want UserPromptSubmit", ContextMetaKey, got)
	}
}

func TestSessionStartContextInjected(t *testing.T) {
	var gotIn SessionInput
	var ended []SessionInput
	cfg := Config{
		SessionStart: []func(context.Context, SessionInput) (SessionOutput, error){
			func(_ context.Context, in SessionInput) (SessionOutput, error) {
				gotIn = in
				return SessionOutput{Context: "project uses tabs"}, nil
			},
		},
		SessionEnd: []func(context.Context, SessionInput) error{
			func(_ context.Context, in SessionInput) error { ended = append(ended, in); return nil },
		},
	}
	final, p := runWith(t, cfg, agent.Config{}, "hello", llmtest.Turn{Text: "hi"})
	mustCompleted(t, final)

	var inRequest bool
	for _, m := range p.Requests[0].Messages {
		inRequest = inRequest || (m.Role == llm.RoleUser && m.Text() == "project uses tabs")
	}
	if !inRequest {
		t.Fatalf("first request %+v has no user message with the context", p.Requests[0].Messages)
	}
	var inState bool
	for _, m := range final.State.Messages() {
		inState = inState || (m.Text() == "project uses tabs" && m.Meta[ContextMetaKey] == "SessionStart")
	}
	if !inState {
		t.Fatalf("State %+v lacks the context message marked %s=SessionStart", final.State.Messages(), ContextMetaKey)
	}
	if gotIn.ThreadID != "th" || gotIn.Resumed || gotIn.Depth != 0 || gotIn.State == nil {
		t.Fatalf("SessionStart input = %+v", gotIn)
	}
	if len(ended) != 1 || ended[0].ThreadID != "th" {
		t.Fatalf("SessionEnd calls = %+v, want one", ended)
	}
}

func TestStopHookForcesContinuation(t *testing.T) {
	var finals []string
	cfg := Config{Stop: []func(context.Context, StopInput) (StopOutput, error){
		func(_ context.Context, in StopInput) (StopOutput, error) {
			finals = append(finals, in.Final)
			if len(finals) == 1 {
				return StopOutput{Continue: "run the tests"}, nil
			}
			return StopOutput{}, nil
		},
	}}
	final, p := runWith(t, cfg, agent.Config{}, "fix it",
		llmtest.Turn{Text: "fixed"}, llmtest.Turn{Text: "tests pass"})
	mustCompleted(t, final)

	if p.Calls() != 2 {
		t.Fatalf("model calls = %d, want 2", p.Calls())
	}
	equal(t, "replies the Stop hook saw", finals, []string{"fixed", "tests pass"})
	var found bool
	for _, m := range final.State.Messages() {
		found = found || (m.Role == llm.RoleUser && m.Text() == "run the tests")
	}
	if !found {
		t.Fatalf("state %+v lacks the injected message", final.State.Messages())
	}
	if last := p.Requests[1].Messages; last[len(last)-1].Text() != "run the tests" {
		t.Fatalf("second request ends with %q, want the injected message", last[len(last)-1].Text())
	}
}

func TestSubagentStartAppendsContext(t *testing.T) {
	rec := &recorder{}
	var in SubagentInput
	cfg := Config{SubagentStart: []func(context.Context, SubagentInput) (SubagentOutput, error){
		func(_ context.Context, got SubagentInput) (SubagentOutput, error) {
			in = got
			return SubagentOutput{Context: "use Go 1.24"}, nil
		},
	}}
	final, _ := runWith(t, cfg, agent.Config{Tools: []tool.Tool{taskStub(rec)}}, "go",
		llmtest.Turn{ToolCalls: []llmtest.Call{call("t", "task", obj{"description": "port it", "subagent_type": "coder"})}},
		llmtest.Turn{Text: "done"})
	mustCompleted(t, final)

	equal(t, "descriptions the task tool saw", rec.all(), []string{"port it\n\nuse Go 1.24"})
	if in.ThreadID != "th" || in.ToolUseID != "t" || in.SubagentType != "coder" || in.Description != "port it" {
		t.Fatalf("SubagentStart input = %+v", in)
	}
}

func TestSubagentStartBlock(t *testing.T) {
	rec := &recorder{}
	cfg := Config{SubagentStart: []func(context.Context, SubagentInput) (SubagentOutput, error){
		func(context.Context, SubagentInput) (SubagentOutput, error) {
			return SubagentOutput{Block: true, Reason: "too deep"}, nil
		},
	}}
	final, _ := runWith(t, cfg, agent.Config{Tools: []tool.Tool{taskStub(rec)}}, "go",
		llmtest.Turn{ToolCalls: []llmtest.Call{call("t", "task", obj{"description": "x", "subagent_type": "coder"})}},
		llmtest.Turn{Text: "done"})
	mustCompleted(t, final)
	if len(rec.all()) != 0 {
		t.Fatalf("task tool ran with %v, want it blocked", rec.all())
	}
	if res := toolResults(final.State)["t"]; !res.IsError || resultText(res) != "Blocked by hook: too deep" {
		t.Fatalf("result = %+v", res)
	}
}

func TestHookErrStopRun(t *testing.T) {
	rec := &recorder{}
	cfg := Config{PreToolUse: []PreToolUseHook{{
		Matcher: Matcher{Tools: []string{"write_file"}},
		Fn: func(context.Context, ToolInput) (PreToolUseOutput, error) {
			return PreToolUseOutput{}, fmt.Errorf("x: %w", agent.ErrStopRun)
		},
	}}}
	final, _ := runWith(t, cfg, agent.Config{Tools: []tool.Tool{readFile(rec), writeFile(rec)}, PermissionMode: permission.ModeBypass}, "go",
		llmtest.Turn{ToolCalls: []llmtest.Call{
			call("w", "write_file", obj{"path": "/a"}), call("r", "read_file", obj{"path": "/b"})}},
		llmtest.Turn{Text: "unreachable"})

	if final.Reason != agent.ReasonHookStopped {
		t.Fatalf("reason = %v (%v), want hook_stopped", final.Reason, final.Err)
	}
	results := toolResults(final.State)
	for _, id := range []string{"w", "r"} {
		if r, ok := results[id]; !ok || !r.IsError {
			t.Fatalf("tool_use %s has result %+v (present=%v), want a paired error result", id, r, ok)
		}
	}
}

// askUser interrupts until it is resumed, like a human-in-the-loop tool.
func askUser() tool.Tool {
	return tool.New("ask_user", "Ask the user.",
		func(_ context.Context, _ pathInput, c *tool.Call) (tool.Result, error) {
			if c.Resume == nil {
				return tool.Result{}, &agent.Interrupt{}
			}
			return tool.Text("answer: " + c.Resume.Message), nil
		}, tool.WithReadOnly())
}

// TestHooksFireAgainWhenAnInterruptedCallResumes pins that the wrapper chain,
// and so the PreToolUse hooks, runs again for a call a human decision resumes.
func TestHooksFireAgainWhenAnInterruptedCallResumes(t *testing.T) {
	var fired int
	cfg := Config{PreToolUse: []PreToolUseHook{{
		Fn: func(context.Context, ToolInput) (PreToolUseOutput, error) { fired++; return PreToolUseOutput{}, nil },
	}}}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{call("q", "ask_user", obj{"path": "colour?"})}},
		llmtest.Turn{Text: "done"})
	a, err := agent.New(agent.Config{Provider: p, Tools: []tool.Tool{askUser()}, Middleware: []agent.Middleware{New(cfg)}})
	if err != nil {
		t.Fatal(err)
	}
	first := drain(t, a.Run(context.Background(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}))
	if first.Reason != agent.ReasonInterrupted || fired != 1 {
		t.Fatalf("after the run: reason=%v fired=%d, want interrupted with one firing", first.Reason, fired)
	}

	resumed := drain(t, a.Resume(context.Background(), "", first.State,
		[]agent.Decision{{ToolUseID: "q", Type: agent.Respond, Message: "blue"}}))
	mustCompleted(t, resumed)
	if fired != 2 {
		t.Fatalf("PreToolUse fired %d times in total, want 2 (once per attempt)", fired)
	}
	if got := resultText(toolResults(resumed.State)["q"]); got != "answer: blue" {
		t.Fatalf("result = %q", got)
	}
}
