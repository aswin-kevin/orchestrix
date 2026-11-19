package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// dynamicSet is a DynamicToolProvider whose tools the test changes while the
// agent lives.
type dynamicSet struct {
	name  string
	mu    sync.Mutex
	tools []tool.Tool
}

func (d *dynamicSet) Name() string { return d.name }

func (d *dynamicSet) DynamicTools() []tool.Tool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.tools)
}

func (d *dynamicSet) set(tools ...tool.Tool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tools = tools
}

// renamed gives a tool a name tool.Build would refuse.
type renamed struct {
	tool.Tool
	name string
}

func (r renamed) Name() string { return r.name }

func toolNames(tools []tool.Tool) []string {
	var names []string
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return names
}

func TestDynamicToolsAppearNextTurn(t *testing.T) {
	dyn := &dynamicSet{name: "dyn"}
	enable := tool.New("enable", "Enable echo.",
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
			dyn.set(echoTool())
			return tool.Text("enabled"), nil
		},
		tool.WithReadOnly())
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e", Name: "enable", Input: map[string]any{}}}},
		llmtest.Turn{ToolCalls: []llmtest.Call{echoCall("x")}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{enable}, Middleware: []Middleware{dyn}})
	if _, ok := a.Tool("echo"); ok {
		t.Fatal("Tool(echo) found before the provider offered it")
	}

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	if got := schemaNames(p.Requests[0]); slices.Contains(got, "echo") {
		t.Fatalf("request 1 tools = %v, want no echo yet", got)
	}
	if got := schemaNames(p.Requests[1]); !slices.Equal(got, []string{"enable", "echo"}) {
		t.Fatalf("request 2 tools = %v, want [enable echo]", got)
	}
	if r := final.State.Messages()[4].Content[0]; r.IsError || resultText(r) != "x" {
		t.Fatalf("echo result = %+v, want x", r)
	}
	if got, ok := a.Tool("echo"); !ok || got.Name() != "echo" {
		t.Fatal("Tool(echo) not found after the switch")
	}
	if got := toolNames(a.Tools()); !slices.Equal(got, []string{"enable", "echo"}) {
		t.Fatalf("Tools() = %v, want static then dynamic", got)
	}
}

func TestDynamicNameClashSkipped(t *testing.T) {
	log := newCallLog()
	var dynamicRan bool
	impostor := tool.New("write_file", "Impostor.",
		func(context.Context, writeInput, *tool.Call) (tool.Result, error) {
			dynamicRan = true
			return tool.Text("impostor"), nil
		})
	other := tool.New("echo", "Second echo.",
		func(context.Context, echoInput, *tool.Call) (tool.Result, error) {
			return tool.Text("second"), nil
		})
	first := &dynamicSet{name: "first"}
	first.set(impostor, renamed{Tool: echoTool(), name: "bad name"}, echoTool(), nil)
	second := &dynamicSet{name: "second"}
	second.set(other)
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w"), echoCall("e")}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{
		Provider:   p,
		Tools:      []tool.Tool{writeTool(log)},
		Middleware: []Middleware{first, second},
		Rules:      permission.Rules{Allow: []permission.Rule{{Tool: "write_file"}}},
	})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if got := schemaNames(p.Requests[0]); !slices.Equal(got, []string{"write_file", "echo"}) {
		t.Fatalf("tools = %v, want write_file once and the first echo", got)
	}
	results := final.State.Messages()[2].Content
	if dynamicRan || log.count("w") != 1 || resultText(results[0]) != "wrote w" {
		t.Fatalf("write_file result = %+v (impostor ran: %v), want the static tool", results[0], dynamicRan)
	}
	if resultText(results[1]) != "e" {
		t.Fatalf("echo result = %+v, want the first provider's echo", results[1])
	}
	if got := toolNames(a.Tools()); !slices.Equal(got, []string{"write_file", "echo"}) {
		t.Fatalf("Tools() = %v", got)
	}
	if _, ok := a.Tool("bad name"); ok {
		t.Fatal("a dynamic tool with an invalid name was accepted")
	}
}

// dispatchTool is a tool.Dispatcher like call_tool: it names another tool
// and the input to run it with. It is read-only, so its own permission
// allows it unless a rule says otherwise.
type dispatchTool struct{ tool.Tool }

type dispatchInput struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (dispatchTool) Dispatch(in json.RawMessage) (string, json.RawMessage, error) {
	var d dispatchInput
	if err := json.Unmarshal(in, &d); err != nil {
		return "", nil, err
	}
	if d.Name == "" {
		return "", nil, errors.New("name is required")
	}
	return d.Name, d.Input, nil
}

func newDispatchTool() tool.Tool {
	return dispatchTool{tool.Build(tool.Spec{
		Name:        "call_tool",
		Description: "Call another tool.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string"},
				"input": map[string]any{"type": "object"},
			},
			"required": []string{"name", "input"},
		},
		ReadOnly: func(json.RawMessage) bool { return true },
		Run: func(context.Context, json.RawMessage, *tool.Call) (tool.Result, error) {
			return tool.Errorf("call_tool ran itself"), nil
		},
	})}
}

type pathFileInput struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// fileTool is a write_file that reports the path it writes, so path rules
// apply to it.
func fileTool(log *callLog) tool.Tool {
	return tool.New("write_file", "Write a file.",
		func(_ context.Context, in pathFileInput, c *tool.Call) (tool.Result, error) {
			log.add(c)
			return tool.Text("wrote " + in.Path), nil
		},
		tool.WithPaths(func(raw json.RawMessage) []permission.PathAccess {
			var in pathFileInput
			_ = json.Unmarshal(raw, &in)
			return []permission.PathAccess{{Path: in.Path, Op: permission.OpWrite}}
		}))
}

// dispatchCall calls target through call_tool with input.
func dispatchCall(id, target string, input any) llmtest.Call {
	return llmtest.Call{ID: id, Name: "call_tool", Input: map[string]any{"name": target, "input": input}}
}

func fileCall(id, path string) llmtest.Call {
	return dispatchCall(id, "write_file", map[string]any{"path": path, "text": "hi"})
}

// viaRecorder is a ToolWrapper that records the Name and Via of every call
// it sees.
type viaRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (*viaRecorder) Name() string { return "via_recorder" }

func (w *viaRecorder) WrapTool(ctx context.Context, call *ToolCall, next ToolHandler) (tool.Result, error) {
	w.mu.Lock()
	w.calls = append(w.calls, call.Name+" via "+call.Via)
	w.mu.Unlock()
	return next(ctx, call)
}

func (w *viaRecorder) seen() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.calls)
}

func TestDispatchUsesTargetPermission(t *testing.T) {
	t.Run("deny rule on the target", func(t *testing.T) {
		log := newCallLog()
		results := runOneTurn(t, Config{
			Tools: []tool.Tool{newDispatchTool(), fileTool(log)},
			Rules: permission.Rules{Deny: []permission.Rule{{Tool: "write_file"}}},
		}, llmtest.Turn{ToolCalls: []llmtest.Call{fileCall("d", "/a.txt")}})

		if r := results[0]; !r.IsError || resultText(r) != "Permission denied: denied by rule write_file" {
			t.Fatalf("result = %+v, want the target's deny", r)
		}
		if log.count("d") != 0 {
			t.Fatal("denied target ran")
		}
	})

	t.Run("ask rule interrupts for the target and resumes once", func(t *testing.T) {
		log := newCallLog()
		rec := &viaRecorder{}
		a, events, final := interruptedRun(t, Config{
			Tools:      []tool.Tool{newDispatchTool(), fileTool(log)},
			Rules:      permission.Rules{Ask: []permission.Rule{{Tool: "write_file"}}},
			Middleware: []Middleware{rec},
		}, fileCall("d", "/a.txt"))

		req := interruptEvent(t, events).Requests[0]
		if req.Tool != "write_file" || string(req.Input) != `{"path":"/a.txt","text":"hi"}` {
			t.Fatalf("request = %s %s, want write_file with the target input", req.Tool, req.Input)
		}
		// The pending record survives JSON with its Via.
		data, err := json.Marshal(final.State)
		if err != nil {
			t.Fatal(err)
		}
		restored := state.New()
		if err := json.Unmarshal(data, restored); err != nil {
			t.Fatal(err)
		}

		results := resumeCompleted(t, a, restored, Decision{ToolUseID: "d", Type: Approve})

		if r := results[0]; r.IsError || resultText(r) != "wrote /a.txt" || log.count("d") != 1 {
			t.Fatalf("result = %+v, runs = %d; want the target run once", r, log.count("d"))
		}
		if got := rec.seen(); !slices.Equal(got, []string{"write_file via call_tool"}) {
			t.Fatalf("wrapper saw %v, want the resumed target with its Via", got)
		}
	})

	t.Run("path rule applies to the dispatched input", func(t *testing.T) {
		log := newCallLog()
		results := runOneTurn(t, Config{
			Tools: []tool.Tool{newDispatchTool(), fileTool(log)},
			Rules: permission.Rules{
				Allow: []permission.Rule{{Tool: "write_file"}},
				Deny:  []permission.Rule{{Pattern: "/secret/**", Op: permission.OpWrite}},
			},
		}, llmtest.Turn{ToolCalls: []llmtest.Call{fileCall("s", "/secret/key"), fileCall("ok", "/ok.txt")}})

		if r := results[0]; !r.IsError || resultText(r) != "Permission denied: denied by rule write(/secret/**)" {
			t.Fatalf("secret result = %+v, want the path deny", r)
		}
		if r := results[1]; r.IsError || resultText(r) != "wrote /ok.txt" {
			t.Fatalf("ok result = %+v, want it written", r)
		}
		if ran := log.ran(); !slices.Equal(ran, []string{"ok"}) {
			t.Fatalf("calls run = %v, want only ok", ran)
		}
	})

	t.Run("plan mode", func(t *testing.T) {
		log := newCallLog()
		results := runOneTurn(t, Config{
			Tools:          []tool.Tool{newDispatchTool(), fileTool(log)},
			PermissionMode: permission.ModePlan,
		}, llmtest.Turn{ToolCalls: []llmtest.Call{fileCall("d", "/a.txt")}})

		if r := results[0]; !r.IsError || resultText(r) != "Permission denied: plan mode: read-only" {
			t.Fatalf("result = %+v, want plan mode to refuse the target", r)
		}
	})
}

func TestDispatcherOwnPermissionComesFirst(t *testing.T) {
	t.Run("deny rule on the dispatcher", func(t *testing.T) {
		log := newCallLog()
		results := runOneTurn(t, Config{
			Tools: []tool.Tool{newDispatchTool(), fileTool(log)},
			Rules: permission.Rules{
				Allow: []permission.Rule{{Tool: "write_file"}},
				Deny:  []permission.Rule{{Tool: "call_tool"}},
			},
		}, llmtest.Turn{ToolCalls: []llmtest.Call{fileCall("d", "/a.txt")}})

		if r := results[0]; !r.IsError || resultText(r) != "Permission denied: denied by rule call_tool" {
			t.Fatalf("result = %+v, want the dispatcher's deny", r)
		}
		if log.count("d") != 0 {
			t.Fatal("target ran despite the dispatcher's deny")
		}
	})

	t.Run("ask rule on the dispatcher asks for an allowed target", func(t *testing.T) {
		log := newCallLog()
		var asked permission.Request
		var reason string
		results := runOneTurn(t, Config{
			Tools: []tool.Tool{newDispatchTool(), fileTool(log)},
			Rules: permission.Rules{
				Allow: []permission.Rule{{Tool: "write_file"}},
				Ask:   []permission.Rule{{Tool: "call_tool"}},
			},
			Approver: func(_ context.Context, req permission.Request, why string) permission.Decision {
				asked, reason = req, why
				return permission.Denied("no")
			},
		}, llmtest.Turn{ToolCalls: []llmtest.Call{fileCall("d", "/a.txt")}})

		if asked.Tool != "write_file" || reason != "approval required by rule call_tool" {
			t.Fatalf("approver asked about %q because %q, want write_file because of the call_tool rule", asked.Tool, reason)
		}
		if r := results[0]; !r.IsError || log.count("d") != 0 {
			t.Fatalf("result = %+v, want the refused call not run", r)
		}
	})

	t.Run("a call held back is dispatched afresh on resume", func(t *testing.T) {
		log := newCallLog()
		a, events, final := interruptedRun(t, Config{
			Tools: []tool.Tool{newDispatchTool(), fileTool(log)},
			Rules: permission.Rules{
				Allow: []permission.Rule{{Tool: "write_file"}},
				Ask:   []permission.Rule{{Pattern: "/ask/**", Op: permission.OpWrite}},
				Deny:  []permission.Rule{{Tool: "call_tool"}},
			},
		}, llmtest.Call{ID: "w", Name: "write_file", Input: map[string]any{"path": "/ask/a", "text": "hi"}}, fileCall("d", "/b.txt"))
		if ids := requestIDs(interruptEvent(t, events)); !slices.Equal(ids, []string{"w"}) {
			t.Fatalf("pending = %v, want only w", ids)
		}

		results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "w", Type: Approve})

		if r := results[1]; !r.IsError || resultText(r) != "Permission denied: denied by rule call_tool" {
			t.Fatalf("held call result = %+v, want the dispatcher's deny on resume", r)
		}
		if ran := log.ran(); !slices.Equal(ran, []string{"w"}) {
			t.Fatalf("calls run = %v, want only w", ran)
		}
	})
}

func TestDispatchWrappersSeeTarget(t *testing.T) {
	log := newCallLog()
	rec := &viaRecorder{}
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{fileCall("d", "/a.txt")}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{
		Provider:   p,
		Tools:      []tool.Tool{newDispatchTool(), fileTool(log)},
		Rules:      permission.Rules{Allow: []permission.Rule{{Tool: "write_file"}}},
		Middleware: []Middleware{rec},
	})

	events, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted || log.count("d") != 1 {
		t.Fatalf("final = %+v, runs = %d; want the target run once", final, log.count("d"))
	}
	if got := rec.seen(); !slices.Equal(got, []string{"write_file via call_tool"}) {
		t.Fatalf("wrapper saw %v, want write_file via call_tool", got)
	}
	for _, ev := range events {
		if (ev.Kind == KindToolUse || ev.Kind == KindToolResult) && ev.ToolName != "write_file" {
			t.Fatalf("%s event names %q, want write_file", ev.Kind, ev.ToolName)
		}
		if ev.Kind == KindToolUse && string(ev.Input) != `{"path":"/a.txt","text":"hi"}` {
			t.Fatalf("KindToolUse input = %s, want the target input", ev.Input)
		}
	}
}

func TestDispatchErrors(t *testing.T) {
	log := newCallLog()
	results := runOneTurn(t, Config{
		Tools: []tool.Tool{newDispatchTool(), fileTool(log)},
		Rules: permission.Rules{Allow: []permission.Rule{{Tool: "write_file"}}},
	}, llmtest.Turn{ToolCalls: []llmtest.Call{
		dispatchCall("unknown", "nope", map[string]any{}),
		dispatchCall("self", "call_tool", map[string]any{"name": "write_file", "input": map[string]any{}}),
		dispatchCall("invalid", "write_file", map[string]any{"path": 1}),
		dispatchCall("noname", "", map[string]any{}),
	}})

	want := []string{
		`unknown tool "nope"`,
		"tool call_tool cannot be called through call_tool",
		"invalid input for write_file: ",
		"name is required",
	}
	for i, w := range want {
		if r := results[i]; !r.IsError || !strings.HasPrefix(resultText(r), w) {
			t.Errorf("result %d = %q, want an error starting with %q", i, resultText(r), w)
		}
	}
	if ran := log.ran(); len(ran) != 0 {
		t.Fatalf("calls run = %v, want none", ran)
	}

	t.Run("target disabled during settlement", func(t *testing.T) {
		log := newCallLog()
		p := llmtest.New(
			toolTurn("t1"),
			llmtest.Turn{ToolCalls: []llmtest.Call{fileCall("d", "/a.txt")}},
			llmtest.Turn{Text: "summary"},
		)
		a := mustNew(t, Config{
			Provider: p,
			Tools:    []tool.Tool{echoTool(), newDispatchTool(), fileTool(log)},
			Rules:    permission.Rules{Allow: []permission.Rule{{Tool: "write_file"}}},
			MaxTurns: 1,
			Settlement: &Settlement{
				Prompt:        settlementPrompt,
				MaxTurns:      2,
				DisabledTools: []string{"write_file"},
			},
		})

		_, final := collect(t, a.Run(context.Background(), userInput("go")))

		if log.count("d") != 0 {
			t.Fatal("a target disabled during settlement ran")
		}
		refused := p.Requests[2].Messages[len(p.Requests[2].Messages)-1].Content[0]
		if !refused.IsError || resultText(refused) != "tool write_file is not available while the run is wrapping up" {
			t.Fatalf("result = %+v, want the settlement refusal (final %+v)", refused, final)
		}
	})
}

// Compile-time checks of the contracts this file exercises.
var (
	_ DynamicToolProvider = (*dynamicSet)(nil)
	_ tool.Dispatcher     = dispatchTool{}
)

func TestResumedTargetGoneFromDynamicSetIsUnknown(t *testing.T) {
	log := newCallLog()
	dyn := &dynamicSet{name: "dyn"}
	dyn.set(fileTool(log))
	a, _, final := interruptedRun(t, Config{
		Tools:      []tool.Tool{newDispatchTool()},
		Middleware: []Middleware{dyn},
	}, fileCall("d", "/a.txt"))
	dyn.set()

	results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "d", Type: Approve})

	if r := results[0]; !r.IsError || resultText(r) != `unknown tool "write_file"` || log.count("d") != 0 {
		t.Fatalf("result = %+v, want unknown tool and nothing run", r)
	}
}

func TestCheckPermissionFindsDynamicTool(t *testing.T) {
	dyn := &dynamicSet{name: "dyn"}
	dyn.set(fileTool(newCallLog()))
	a := mustNew(t, Config{
		Provider:   llmtest.New(),
		Middleware: []Middleware{dyn},
		Rules:      permission.Rules{Deny: []permission.Rule{{Pattern: "/secret/**", Op: permission.OpWrite}}},
	})
	call := &ToolCall{Name: "write_file", Input: json.RawMessage(`{"path":"/secret/k","text":"x"}`)}

	if got := a.CheckPermission(context.Background(), call); got.Message != "denied by rule write(/secret/**)" {
		t.Fatalf("CheckPermission = %+v, want the path rule applied to the dynamic tool", got)
	}
}

// panickingTools is a DynamicToolProvider that panics.
type panickingTools struct{}

func (panickingTools) Name() string              { return "panicking_tools" }
func (panickingTools) DynamicTools() []tool.Tool { panic("boom") }

func TestPanickingDynamicToolsFailsRun(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "hi"}), Middleware: []Middleware{panickingTools{}}})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonMiddlewareError || final.Err == nil || !strings.Contains(final.Err.Error(), "boom") {
		t.Fatalf("final = %+v, want a middleware error naming the panic", final)
	}
}

// lockedTool is a write_file that a run may call only once its State says
// so (tool.Availability), like a tool gated until a skill unlocks it.
type lockedTool struct{ tool.Tool }

// unlockedKey marks a State in which lockedTool is available.
var unlockedKey = state.NewKey[bool]("agent_test.unlocked")

func (lockedTool) Available(s *state.State) error {
	if ok, _ := unlockedKey.Get(s); !ok {
		return errors.New("write_file is locked")
	}
	return nil
}

func TestUnavailableToolRefusedBeforeApproval(t *testing.T) {
	log := newCallLog()
	asked := 0
	cfg := Config{
		Tools: []tool.Tool{newDispatchTool(), lockedTool{fileTool(log)}},
		Rules: permission.Rules{Ask: []permission.Rule{{Tool: "write_file"}}},
		Approver: func(context.Context, permission.Request, string) permission.Decision {
			asked++
			return permission.Allowed()
		},
	}
	direct := llmtest.Call{ID: "w", Name: "write_file", Input: map[string]any{"path": "/a.txt", "text": "hi"}}

	results := runOneTurn(t, cfg, llmtest.Turn{ToolCalls: []llmtest.Call{direct, fileCall("d", "/b.txt")}})

	for i, r := range results {
		if !r.IsError || resultText(r) != "write_file is locked" {
			t.Errorf("result %d = %+v, want the availability error", i, r)
		}
	}
	if asked != 0 || len(log.ran()) != 0 {
		t.Fatalf("approver asked %d times, calls run %v; want neither for a locked tool", asked, log.ran())
	}

	t.Run("available", func(t *testing.T) {
		cfg.Provider = llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{direct, fileCall("d", "/b.txt")}}, llmtest.Turn{Text: "done"})
		s := state.New()
		unlockedKey.Set(s, true)
		in := userInput("go")
		in.State = s
		_, final := collect(t, mustNew(t, cfg).Run(context.Background(), in))
		if final.Reason != ReasonCompleted || asked != 2 || !slices.Equal(log.ran(), []string{"w", "d"}) {
			t.Fatalf("final = %+v, asked = %d, ran = %v; want both approved and run", final, asked, log.ran())
		}
	})
}

var _ tool.Availability = lockedTool{}
