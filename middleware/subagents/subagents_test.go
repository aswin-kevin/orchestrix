package subagents

import (
	"context"
	"encoding/json"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

type obj = map[string]any

// Test state keys: one shared with children, one private to its run.
var (
	sharedKey  = state.NewKey[string]("subagentstest.shared")
	privateKey = state.NewKey[string]("subagentstest.private", state.Private[string]())
)

func taskCall(id, typ, description string) llmtest.Call {
	return llmtest.Call{ID: id, Name: ToolTask, Input: obj{"description": description, "subagent_type": typ}}
}

func newMiddleware(t *testing.T, cfg Config) agent.Middleware {
	t.Helper()
	mw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mw
}

func newParent(t *testing.T, p llm.Provider, mw agent.Middleware, tools ...tool.Tool) *agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Config{
		Provider:       p,
		System:         []string{"parent persona"},
		Tools:          tools,
		Middleware:     []agent.Middleware{mw},
		PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

// collect runs a to completion and returns every event, the Final last.
func collect(t *testing.T, ctx context.Context, a *agent.Agent, in agent.Input) ([]agent.Event, *agent.Final) {
	t.Helper()
	var events []agent.Event
	var final *agent.Final
	for ev := range a.Run(ctx, in) {
		events = append(events, ev)
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatal("run ended without a final event")
	}
	return events, final
}

func userInput(text string) agent.Input {
	return agent.Input{ThreadID: "thread", Messages: []llm.Message{llm.UserText(text)}}
}

// toolResults returns the tool_result blocks of the last message of req, in order.
func toolResults(req llm.Request) []llm.Block {
	var out []llm.Block
	for _, b := range req.Messages[len(req.Messages)-1].Content {
		if b.Type == llm.BlockToolResult {
			out = append(out, b)
		}
	}
	return out
}

func blockText(b llm.Block) string {
	var parts []string
	for _, c := range b.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

// onlyResult returns the single tool_result the parent sent in its second request.
func onlyResult(t *testing.T, parent *llmtest.Scripted) llm.Block {
	t.Helper()
	if len(parent.Requests) < 2 {
		t.Fatalf("parent made %d requests, want at least 2", len(parent.Requests))
	}
	results := toolResults(parent.Requests[1])
	if len(results) != 1 {
		t.Fatalf("parent got %d tool results, want 1", len(results))
	}
	return results[0]
}

func toolNames(req llm.Request) []string {
	var names []string
	for _, ts := range req.Tools {
		names = append(names, ts.Name)
	}
	return names
}

type emptyInput struct{}

type sleepInput struct{}

func sleepTool(d time.Duration) tool.Tool {
	return tool.New("sleep", "Sleeps.", func(ctx context.Context, _ sleepInput, _ *tool.Call) (tool.Result, error) {
		select {
		case <-time.After(d):
			return tool.Text("slept"), nil
		case <-ctx.Done():
			return tool.Result{}, ctx.Err()
		}
	}, tool.WithConcurrent())
}

func TestIsolatedChildSeesOnlyTask(t *testing.T) {
	child := llmtest.New(llmtest.Turn{Text: "the answer is 42"})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "researcher", "find the answer")}},
		llmtest.Turn{Text: "done"},
	)
	mw := newMiddleware(t, Config{Subagents: []Subagent{{
		Name: "researcher", Description: "Researches.", SystemPrompt: "you research", Provider: child,
	}}})
	_, final := collect(t, context.Background(), newParent(t, parent, mw), userInput("parent history that must stay hidden"))

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	if child.Calls() != 1 {
		t.Fatalf("child made %d requests, want 1", child.Calls())
	}
	first := child.Requests[0]
	if len(first.Messages) != 1 || first.Messages[0].Role != llm.RoleUser || first.Messages[0].Text() != "find the answer" {
		t.Fatalf("child messages = %+v, want exactly the task description", first.Messages)
	}
	if len(first.System) == 0 || first.System[0] != "you research" || slices.Contains(first.System, "parent persona") {
		t.Fatalf("child system = %q, want the subagent's own prompt first and no parent persona", first.System)
	}
	if res := onlyResult(t, parent); res.IsError || blockText(res) != "the answer is 42" {
		t.Fatalf("task result = %q (error=%v), want the child's final text", blockText(res), res.IsError)
	}
}

func TestTwoTaskCallsRunInParallel(t *testing.T) {
	childA := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s", Name: "sleep"}}}, llmtest.Turn{Text: "from a"})
	childB := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s", Name: "sleep"}}}, llmtest.Turn{Text: "from b"})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("ta", "a", "do a"), taskCall("tb", "b", "do b")}},
		llmtest.Turn{Text: "done"},
	)
	sleeper := []tool.Tool{sleepTool(150 * time.Millisecond)}
	mw := newMiddleware(t, Config{Subagents: []Subagent{
		{Name: "a", Description: "A.", SystemPrompt: "a", Provider: childA, Tools: sleeper},
		{Name: "b", Description: "B.", SystemPrompt: "b", Provider: childB, Tools: sleeper},
	}})

	start := time.Now()
	_, final := collect(t, context.Background(), newParent(t, parent, mw), userInput("go"))
	elapsed := time.Since(start)

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	if elapsed >= 280*time.Millisecond {
		t.Fatalf("two 150ms subagents took %v, want them to run in parallel (< 280ms)", elapsed)
	}
	results := toolResults(parent.Requests[1])
	if len(results) != 2 || results[0].ToolUseID != "ta" || blockText(results[0]) != "from a" ||
		results[1].ToolUseID != "tb" || blockText(results[1]) != "from b" {
		t.Fatalf("results = %+v, want from a then from b in call order", results)
	}
}

func TestChildEventsForwardedWithAgentName(t *testing.T) {
	child := llmtest.New(llmtest.Turn{Text: "child speaking"})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "researcher", "speak")}},
		llmtest.Turn{Text: "parent done"},
	)
	mw := newMiddleware(t, Config{Subagents: []Subagent{{
		Name: "researcher", Description: "Researches.", SystemPrompt: "you research", Provider: child,
	}}})
	events, _ := collect(t, context.Background(), newParent(t, parent, mw), userInput("go"))

	var finals int
	var forwarded bool
	for _, ev := range events {
		if ev.Kind == agent.KindFinal {
			finals++
		}
		if ev.Kind == agent.KindText && ev.Text == "child speaking" {
			forwarded = ev.Agent == "researcher" && ev.ParentToolID == "t1"
		}
	}
	if !forwarded {
		t.Fatal("child text not forwarded with Agent=researcher and ParentToolID=t1")
	}
	if finals != 1 {
		t.Fatalf("got %d final events, want exactly the parent's", finals)
	}
}

type writeInput struct{}

// writerTool writes /out.md to the call's backend and sets both test keys.
func writerTool() tool.Tool {
	return tool.New("writer", "Writes.", func(ctx context.Context, _ writeInput, c *tool.Call) (tool.Result, error) {
		if err := c.Backend.Write(ctx, "/out.md", []byte("report")); err != nil {
			return tool.Result{}, err
		}
		sharedKey.Set(c.State, "from child")
		privateKey.Set(c.State, "child secret")
		return tool.Text("written"), nil
	})
}

func TestFilesMergeBackViaSharedBackend(t *testing.T) {
	child := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w", Name: "writer"}}}, llmtest.Turn{Text: "wrote it"})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "writer", "write the report")}},
		llmtest.Turn{Text: "done"},
	)
	mw := newMiddleware(t, Config{Subagents: []Subagent{{
		Name: "writer", Description: "Writes.", SystemPrompt: "you write", Provider: child, Tools: []tool.Tool{writerTool()},
	}}})
	be := memory.New()
	a, err := agent.New(agent.Config{
		Provider: parent, Backend: be, Middleware: []agent.Middleware{mw}, PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := state.New()
	privateKey.Set(s, "parent secret")
	in := userInput("go")
	in.State = s
	_, final := collect(t, context.Background(), a, in)

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	got, err := be.Read(context.Background(), "/out.md", 0, 0)
	if err != nil || !slices.Equal(got.Lines, []string{"report"}) {
		t.Fatalf("parent backend /out.md = %+v, %v; want the child's file", got, err)
	}
	if v, _ := sharedKey.Get(final.State); v != "from child" {
		t.Fatalf("shared key = %q, want the child's value merged back", v)
	}
	if v, _ := privateKey.Get(final.State); v != "parent secret" {
		t.Fatalf("private key = %q, want the parent's own value kept", v)
	}
}

func TestDepthGuard(t *testing.T) {
	// Middleware is nil, so the child inherits the parent's stack, task tool included.
	child := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("inner", "researcher", "go deeper")}},
		llmtest.Turn{Text: "could not go deeper"},
	)
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "researcher", "go")}},
		llmtest.Turn{Text: "done"},
	)
	mw := newMiddleware(t, Config{MaxDepth: 1, Subagents: []Subagent{{
		Name: "researcher", Description: "Researches.", SystemPrompt: "you research", Provider: child,
	}}})
	_, final := collect(t, context.Background(), newParent(t, parent, mw), userInput("go"))

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	if child.Calls() != 2 {
		t.Fatalf("child made %d requests, want 2 (no grandchild ran)", child.Calls())
	}
	results := toolResults(child.Requests[1])
	if len(results) != 1 || !results[0].IsError || !strings.Contains(blockText(results[0]), "depth") {
		t.Fatalf("nested task result = %+v, want an error mentioning depth", results)
	}
}

type echoInput struct {
	Text string `json:"text"`
}

func echoTool() tool.Tool {
	return tool.New("echo", "Echoes.", func(_ context.Context, in echoInput, _ *tool.Call) (tool.Result, error) {
		return tool.Text(in.Text), nil
	})
}

func TestGeneralPurposeInheritsParentTools(t *testing.T) {
	// Parent and general-purpose child share the parent's provider; the
	// calls are sequential, so the script order is deterministic.
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", GeneralPurpose, "look around")}},
		llmtest.Turn{Text: "gp done"},
		llmtest.Turn{Text: "done"},
	)
	_, final := collect(t, context.Background(), newParent(t, p, newMiddleware(t, Config{}), echoTool()), userInput("go"))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v), want completed", final.Reason, final.Err)
	}
	gp := p.Requests[1]
	if names := toolNames(gp); !slices.Contains(names, "echo") || !slices.Contains(names, ToolTask) {
		t.Fatalf("general-purpose tools = %v, want echo and task", names)
	}
	if len(gp.System) == 0 || gp.System[0] != DefaultGeneralPurposePrompt {
		t.Fatalf("general-purpose system = %q, want DefaultGeneralPurposePrompt first", gp.System)
	}
	if res := toolResults(p.Requests[2]); len(res) != 1 || blockText(res[0]) != "gp done" {
		t.Fatalf("task result = %+v, want gp done", res)
	}

	// A spec named general-purpose replaces the built-in one.
	custom := llmtest.New(llmtest.Turn{Text: "custom done"})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", GeneralPurpose, "look around")}},
		llmtest.Turn{Text: "done"},
	)
	mw := newMiddleware(t, Config{Subagents: []Subagent{{
		Name: GeneralPurpose, Description: "Custom general.", SystemPrompt: "custom general prompt", Provider: custom,
	}}})
	collect(t, context.Background(), newParent(t, parent, mw, echoTool()), userInput("go"))
	if custom.Calls() != 1 || custom.Requests[0].System[0] != "custom general prompt" {
		t.Fatalf("custom general-purpose not used: calls=%d", custom.Calls())
	}
	if res := onlyResult(t, parent); blockText(res) != "custom done" {
		t.Fatalf("task result = %q, want custom done", blockText(res))
	}
}

func structuredSpec(p llm.Provider) Subagent {
	return Subagent{
		Name: "counter", Description: "Counts.", SystemPrompt: "you count", Provider: p,
		OutputSchema: obj{
			"type":       "object",
			"properties": obj{"n": obj{"type": "integer"}},
			"required":   []any{"n"},
		},
	}
}

func TestStructuredOutputRetriesThenSucceeds(t *testing.T) {
	child := llmtest.New(llmtest.Turn{Text: "not json"}, llmtest.Turn{Text: "```json\n{\"n\": 1}\n```"})
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "counter", "count")}},
		llmtest.Turn{Text: "done"},
	)
	collect(t, context.Background(), newParent(t, parent, newMiddleware(t, Config{Subagents: []Subagent{structuredSpec(child)}})), userInput("go"))

	if res := onlyResult(t, parent); res.IsError || blockText(res) != `{"n":1}` {
		t.Fatalf("task result = %q (error=%v), want the compact JSON", blockText(res), res.IsError)
	}
	if child.Calls() != 2 {
		t.Fatalf("child made %d requests, want 2", child.Calls())
	}
	retry := child.Requests[1].Messages
	if last := retry[len(retry)-1].Text(); !strings.HasPrefix(last, "Your answer is not valid: ") ||
		!strings.HasSuffix(last, ". Reply with only the JSON object.") {
		t.Fatalf("retry prompt = %q", last)
	}
	if !strings.Contains(strings.Join(child.Requests[0].System, "\n"), `"required"`) {
		t.Fatalf("child system %q does not describe the output schema", child.Requests[0].System)
	}

	// Always invalid: one attempt plus DefaultOutputRetries, then an error result.
	bad := llmtest.New(llmtest.Turn{Text: "nope"}, llmtest.Turn{Text: `{"n":"x"}`}, llmtest.Turn{Text: "still no"})
	parent = llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", "counter", "count")}},
		llmtest.Turn{Text: "done"},
	)
	collect(t, context.Background(), newParent(t, parent, newMiddleware(t, Config{Subagents: []Subagent{structuredSpec(bad)}})), userInput("go"))
	if bad.Calls() != 1+DefaultOutputRetries {
		t.Fatalf("child made %d requests, want %d", bad.Calls(), 1+DefaultOutputRetries)
	}
	if res := onlyResult(t, parent); !res.IsError {
		t.Fatalf("task result = %q, want an error result", blockText(res))
	}
}

func TestCancelStopsParallelChildren(t *testing.T) {
	baseline := runtime.NumGoroutine()

	var started sync.WaitGroup
	started.Add(2)
	block := tool.New("block", "Blocks until cancelled.", func(ctx context.Context, _ emptyInput, _ *tool.Call) (tool.Result, error) {
		started.Done()
		<-ctx.Done()
		return tool.Result{}, ctx.Err()
	}, tool.WithConcurrent())
	childTurn := llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "b", Name: "block"}}}
	parent := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("ta", "a", "a"), taskCall("tb", "b", "b")}})
	mw := newMiddleware(t, Config{Subagents: []Subagent{
		{Name: "a", Description: "A.", SystemPrompt: "a", Provider: llmtest.New(childTurn), Tools: []tool.Tool{block}},
		{Name: "b", Description: "B.", SystemPrompt: "b", Provider: llmtest.New(childTurn), Tools: []tool.Tool{block}},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		started.Wait()
		cancel()
	}()
	_, final := collect(t, ctx, newParent(t, parent, mw), userInput("go"))

	if final.Reason != agent.ReasonAbortedTools {
		t.Fatalf("reason = %s (%v), want aborted_tools", final.Reason, final.Err)
	}
	msgs := final.State.Messages()
	last := msgs[len(msgs)-1]
	var paired []string
	for _, b := range last.Content {
		if b.Type == llm.BlockToolResult {
			paired = append(paired, b.ToolUseID)
		}
	}
	if !slices.Equal(paired, []string{"ta", "tb"}) {
		t.Fatalf("tool results for %v, want both task calls paired", paired)
	}

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d, want back to baseline %d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUnknownSubagentType(t *testing.T) {
	mw := newMiddleware(t, Config{})
	tools := mw.(agent.ToolProvider).Tools()
	if len(tools) != 1 || tools[0].Name() != ToolTask {
		t.Fatalf("tools = %v, want the task tool only", tools)
	}
	task := tools[0]

	// The schema enumerates the configured types, so the agent rejects an
	// unknown one before the tool runs.
	if err := tool.Validate(task.Schema(), json.RawMessage(`{"description":"x","subagent_type":"nobody"}`)); err == nil {
		t.Fatal("schema accepted an unknown subagent_type")
	}
	// The tool itself refuses too, for callers that skip validation.
	res, err := task.Call(context.Background(), json.RawMessage(`{"description":"x","subagent_type":"nobody"}`), &tool.Call{State: state.New()})
	if err != nil || !res.IsError || !strings.Contains(res.Flatten(), `"nobody"`) {
		t.Fatalf("result = %q (error=%v), err = %v; want an error result naming the type", res.Flatten(), res.IsError, err)
	}
}

func TestReservedForkName(t *testing.T) {
	if _, err := New(Config{Subagents: []Subagent{{Name: Fork, Description: "x", SystemPrompt: "x"}}}); err == nil {
		t.Fatal("New accepted a subagent named fork")
	}
}

func TestNewRejectsInvalidSpecs(t *testing.T) {
	valid := Subagent{Name: "ok", Description: "d", SystemPrompt: "p"}
	cases := map[string]Config{
		"bad name":            {Subagents: []Subagent{{Name: "has space", Description: "d", SystemPrompt: "p"}}},
		"missing description": {Subagents: []Subagent{{Name: "x", SystemPrompt: "p"}}},
		"missing prompt":      {Subagents: []Subagent{{Name: "x", Description: "d"}}},
		"duplicate":           {Subagents: []Subagent{valid, valid}},
		"negative depth":      {MaxDepth: -1},
		"no subagents":        {DisableGeneralPurpose: true},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted an invalid config")
			}
		})
	}
}

func TestToolIsConcurrentAndAllowed(t *testing.T) {
	task := newMiddleware(t, Config{}).(agent.ToolProvider).Tools()[0]
	in := json.RawMessage(`{"description":"x","subagent_type":"general-purpose"}`)
	if !task.ConcurrencySafe(in) {
		t.Fatal("task tool is not concurrency-safe")
	}
	if d := task.CheckPermission(context.Background(), in, permission.Context{}); d.Behavior != permission.Allow {
		t.Fatalf("task permission = %+v, want allowed", d)
	}
}

func TestPromptAppendedAfterSystem(t *testing.T) {
	wrap := func(cfg Config) []string {
		var got []string
		req := &agent.ModelRequest{Request: llm.Request{System: []string{"persona"}}}
		_, _ = newMiddleware(t, cfg).(agent.ModelWrapper).WrapModel(context.Background(), req,
			func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
				got = r.Request.System
				return &llm.Response{}, nil
			})
		return got
	}
	if got := wrap(Config{}); !slices.Equal(got, []string{"persona", DefaultPrompt}) {
		t.Fatalf("system = %q, want persona then DefaultPrompt", got)
	}
	off := ""
	if got := wrap(Config{Prompt: &off}); !slices.Equal(got, []string{"persona"}) {
		t.Fatalf("system = %q, want the prompt disabled", got)
	}
}

func TestTaskToolNeedsAllowRuleInDontAskMode(t *testing.T) {
	allowTask, err := permission.ParseRule(ToolTask)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		rules   permission.Rules
		allowed bool
	}{
		{"no rule", permission.Rules{}, false},
		{"allow rule", permission.Rules{Allow: []permission.Rule{allowTask}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := llmtest.New(
				llmtest.Turn{ToolCalls: []llmtest.Call{taskCall("t1", GeneralPurpose, "do it")}},
				llmtest.Turn{Text: "child answer"},
				llmtest.Turn{Text: "done"},
			)
			a, err := agent.New(agent.Config{
				Provider: p, Middleware: []agent.Middleware{newMiddleware(t, Config{})},
				PermissionMode: permission.ModeDontAsk, Rules: tc.rules,
			})
			if err != nil {
				t.Fatal(err)
			}
			collect(t, context.Background(), a, userInput("go"))
			// The child shares the provider, so the parent's last request
			// holds the task result.
			results := toolResults(p.Requests[len(p.Requests)-1])
			if len(results) != 1 {
				t.Fatalf("parent got %d tool results, want 1", len(results))
			}
			res := results[0]
			if allowed := !res.IsError; allowed != tc.allowed {
				t.Fatalf("task result = %q (error %v), want allowed=%v", blockText(res), res.IsError, tc.allowed)
			}
		})
	}
}
