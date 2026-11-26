package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// runOneTurn runs an agent whose first turn makes calls and whose second turn
// answers "done", and returns the tool_result blocks of the first turn.
func runOneTurn(t *testing.T, cfg Config, first llmtest.Turn) []llm.Block {
	t.Helper()
	cfg.Provider = llmtest.New(first, llmtest.Turn{Text: "done"})
	_, final := collect(t, mustNew(t, cfg).Run(t.Context(), userInput("go")))
	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	return final.State.Messages()[2].Content
}

func resultText(b llm.Block) string {
	var parts []string
	for _, c := range b.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "")
}

type xInput struct {
	X int `json:"x"`
}

func TestUnknownToolAndInvalidJSON(t *testing.T) {
	var got []int
	xTool := tool.New("x_tool", "Record x.",
		func(_ context.Context, in xInput, _ *tool.Call) (tool.Result, error) {
			got = append(got, in.X)
			return tool.Text("ok"), nil
		},
		tool.WithReadOnly())

	results := runOneTurn(t, Config{Tools: []tool.Tool{xTool}}, llmtest.Turn{
		RawToolInput: true,
		ToolCalls: []llmtest.Call{
			{ID: "u", Name: "nope", Input: `{}`},
			{ID: "bad", Name: "x_tool", Input: `{"a":`},
			{ID: "fenced", Name: "x_tool", Input: "```json\n{\"x\":1}\n```"},
			{ID: "schema", Name: "x_tool", Input: `{"x":"one"}`},
		},
	})

	if r := results[0]; !r.IsError || resultText(r) != `unknown tool "nope"` {
		t.Fatalf("unknown tool result = %+v", r)
	}
	if r := results[1]; !r.IsError || !strings.HasPrefix(resultText(r), "invalid JSON arguments: ") ||
		!strings.HasSuffix(resultText(r), ". Re-issue the call with valid JSON.") {
		t.Fatalf("invalid JSON result = %+v", r)
	}
	if r := results[2]; r.IsError || resultText(r) != "ok" {
		t.Fatalf("fenced JSON result = %+v, want the tool to run", r)
	}
	if r := results[3]; !r.IsError || !strings.Contains(resultText(r), "x: expected integer") {
		t.Fatalf("schema-invalid result = %+v, want the validation message", r)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("tool ran with %v, want exactly [1]", got)
	}
}

func TestRawInputerSkipsSchemaValidation(t *testing.T) {
	var got string
	raw := tool.Build(tool.Spec{
		Name:     "raw",
		Schema:   map[string]any{"type": "object", "required": []string{"must"}},
		RawInput: true,
		ReadOnly: func(json.RawMessage) bool { return true },
		Run: func(_ context.Context, in json.RawMessage, _ *tool.Call) (tool.Result, error) {
			got = string(in)
			return tool.Text("ok"), nil
		},
	})

	results := runOneTurn(t, Config{Tools: []tool.Tool{raw}}, llmtest.Turn{
		ToolCalls: []llmtest.Call{{ID: "r", Name: "raw", Input: map[string]any{"other": true}}},
	})

	if results[0].IsError || got != `{"other":true}` {
		t.Fatalf("result = %+v, input = %s; want the raw input passed through", results[0], got)
	}
}

type writeInput struct {
	Text string `json:"text"`
}

func TestPermissionDenyAndApprover(t *testing.T) {
	var received []string
	write := tool.New("write_file", "Write.",
		func(_ context.Context, in writeInput, _ *tool.Call) (tool.Result, error) {
			received = append(received, in.Text)
			return tool.Text("wrote " + in.Text), nil
		})
	turn := llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w", Name: "write_file", Input: map[string]any{"text": "original"}}}}

	t.Run("deny rule", func(t *testing.T) {
		received = nil
		results := runOneTurn(t, Config{
			Tools: []tool.Tool{write},
			Rules: permission.Rules{Deny: []permission.Rule{{Tool: "write_file"}}},
			Approver: func(context.Context, permission.Request, string) permission.Decision {
				t.Fatal("approver consulted for a denied call")
				return permission.Decision{}
			},
		}, turn)
		if r := results[0]; !r.IsError || !strings.HasPrefix(resultText(r), "Permission denied: ") {
			t.Fatalf("result = %+v, want permission denied", r)
		}
		if len(received) != 0 {
			t.Fatal("denied tool ran")
		}
	})

	t.Run("approver rewrites input", func(t *testing.T) {
		received = nil
		var asked permission.Request
		results := runOneTurn(t, Config{
			Tools: []tool.Tool{write},
			Approver: func(_ context.Context, req permission.Request, _ string) permission.Decision {
				asked = req
				return permission.Decision{Behavior: permission.Allow, UpdatedInput: json.RawMessage(`{"text":"approved"}`)}
			},
		}, turn)
		if r := results[0]; r.IsError || resultText(r) != "wrote approved" {
			t.Fatalf("result = %+v, want the tool to run with updated input", r)
		}
		if len(received) != 1 || received[0] != "approved" {
			t.Fatalf("tool received %v, want [approved]", received)
		}
		if asked.Tool != "write_file" || string(asked.Input) != `{"text":"original"}` {
			t.Fatalf("approver request = %+v", asked)
		}
	})

	t.Run("approver denies", func(t *testing.T) {
		received = nil
		results := runOneTurn(t, Config{
			Tools: []tool.Tool{write},
			Approver: func(context.Context, permission.Request, string) permission.Decision {
				return permission.Denied("not today")
			},
		}, turn)
		if r := results[0]; !r.IsError || resultText(r) != "Permission denied: not today" {
			t.Fatalf("result = %+v, want permission denied by approver", r)
		}
	})

	t.Run("approver input revalidated", func(t *testing.T) {
		received = nil
		results := runOneTurn(t, Config{
			Tools: []tool.Tool{write},
			Approver: func(context.Context, permission.Request, string) permission.Decision {
				return permission.Decision{Behavior: permission.Allow, UpdatedInput: json.RawMessage(`{"text":5}`)}
			},
		}, turn)
		if r := results[0]; !r.IsError || !strings.Contains(resultText(r), "text: expected string") {
			t.Fatalf("result = %+v, want a validation error", r)
		}
		if len(received) != 0 {
			t.Fatal("tool ran with invalid approver input")
		}
	})
}

func TestPermissionRequestFromOptionalInterfaces(t *testing.T) {
	var got permission.Request
	paths := []permission.PathAccess{{Path: "/a.txt", Op: permission.OpWrite}}
	editor := tool.New("edit", "Edit.",
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) { return tool.Text("ok"), nil },
		tool.WithEdit(),
		tool.WithShellSubject(func(json.RawMessage) string { return "subject" }),
		tool.WithPaths(func(json.RawMessage) []permission.PathAccess { return paths }),
		tool.WithPermission(func(_ context.Context, _ json.RawMessage, pc permission.Context) permission.Decision {
			if pc.Mode != permission.ModeAcceptEdits {
				return permission.Denied("wrong mode in permission context")
			}
			return permission.AskUser("self check")
		}))

	runOneTurn(t, Config{
		Tools:          []tool.Tool{editor},
		PermissionMode: permission.ModeAcceptEdits,
		Rules:          permission.Rules{Ask: []permission.Rule{{Tool: "edit"}}},
		Approver: func(_ context.Context, req permission.Request, _ string) permission.Decision {
			got = req
			return permission.Allowed()
		},
	}, llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e", Name: "edit"}}})

	if !got.Edit || got.ReadOnly || got.Subject != "subject" || !got.Shell || len(got.Paths) != 1 || got.Paths[0] != paths[0] {
		t.Fatalf("permission request = %+v", got)
	}
	if got.SelfCheck.Behavior != permission.Ask || got.SelfCheck.Message != "self check" {
		t.Fatalf("self check = %+v", got.SelfCheck)
	}
}

func TestToolPanicRecovered(t *testing.T) {
	boom := tool.New("boom", "Panic.",
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) { panic("kaboom") },
		tool.WithReadOnly())

	results := runOneTurn(t, Config{Tools: []tool.Tool{boom, echoTool()}}, llmtest.Turn{ToolCalls: []llmtest.Call{
		{ID: "p", Name: "boom"},
		{ID: "e", Name: "echo", Input: map[string]any{"text": "still here"}},
	}})

	if r := results[0]; !r.IsError || !strings.Contains(resultText(r), "kaboom") {
		t.Fatalf("panic result = %+v", r)
	}
	if r := results[1]; r.IsError || resultText(r) != "still here" {
		t.Fatalf("following call result = %+v", r)
	}
}

func TestToolGoErrorBecomesErrorResult(t *testing.T) {
	failing := tool.New("fail", "Fail.",
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
			return tool.Result{}, context.DeadlineExceeded
		},
		tool.WithReadOnly())

	results := runOneTurn(t, Config{Tools: []tool.Tool{failing}}, llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "f", Name: "fail"}}})

	if r := results[0]; !r.IsError || !strings.Contains(resultText(r), "deadline exceeded") {
		t.Fatalf("result = %+v, want an error result carrying the Go error", r)
	}
}

func TestToolResultTruncatedAcrossBlocks(t *testing.T) {
	multi := tool.New("multi", "Two text blocks.",
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
			return tool.Result{Content: []llm.Block{llm.TextBlock("abcdef"), llm.TextBlock("ghij")}}, nil
		},
		tool.WithReadOnly())

	results := runOneTurn(t, Config{Tools: []tool.Tool{multi}, MaxToolOutputChars: 4},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "m", Name: "multi"}}})

	blocks := results[0].Content
	if blocks[0].Text != tool.Truncate("abcdef", 4) {
		t.Fatalf("first block = %q", blocks[0].Text)
	}
	if strings.Contains(blocks[1].Text, "ghij") || !strings.Contains(blocks[1].Text, "truncated 4 chars") {
		t.Fatalf("second block = %q, want it dropped with a marker", blocks[1].Text)
	}
}

func TestExtraMessagesFollowToolResults(t *testing.T) {
	withExtra := func(name string) tool.Tool {
		return tool.New(name, "Attach an extra message.",
			func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
				r := tool.Text(name)
				r.Extra = []llm.Message{llm.UserText("extra from " + name)}
				return r, nil
			},
			tool.WithReadOnly())
	}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "1", Name: "one"}, {ID: "2", Name: "two"}}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{withExtra("one"), withExtra("two")}})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	msgs := final.State.Messages()
	if len(msgs) != 6 {
		t.Fatalf("state has %d messages, want 6", len(msgs))
	}
	if len(msgs[2].Content) != 2 || msgs[2].Content[0].ToolUseID != "1" || msgs[2].Content[1].ToolUseID != "2" {
		t.Fatalf("tool result message = %+v", msgs[2])
	}
	if msgs[3].Text() != "extra from one" || msgs[4].Text() != "extra from two" {
		t.Fatalf("extra messages = %q, %q", msgs[3].Text(), msgs[4].Text())
	}
}

func TestProgressEvents(t *testing.T) {
	chatty := tool.New("chatty", "Report progress.",
		func(_ context.Context, _ struct{}, c *tool.Call) (tool.Result, error) {
			c.Progress("halfway")
			return tool.Text("ok"), nil
		},
		tool.WithReadOnly())
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c", Name: "chatty"}}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{chatty}})

	events, _ := collect(t, a.Run(t.Context(), userInput("go")))

	for _, ev := range events {
		if ev.Kind == KindProgress && ev.Text == "halfway" && ev.ToolID == "c" && ev.ToolName == "chatty" {
			return
		}
	}
	t.Fatal("no KindProgress event for the tool's progress message")
}

func TestBatches(t *testing.T) {
	calls := []*toolCall{
		{concurrent: true}, {concurrent: true}, {concurrent: false},
		{concurrent: true}, {concurrent: false}, {concurrent: false}, {concurrent: true},
	}

	var sizes []int
	for _, b := range batches(calls) {
		sizes = append(sizes, len(b))
	}

	want := []int{2, 1, 1, 1, 1, 1}
	if len(sizes) != len(want) {
		t.Fatalf("batch sizes = %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("batch sizes = %v, want %v", sizes, want)
		}
	}
}

func TestParseToolInput(t *testing.T) {
	tests := []struct {
		raw, want string
		ok        bool
	}{
		{`{"x":1}`, `{"x":1}`, true},
		{"```json\n{\"x\":1}\n```", `{"x":1}`, true},
		{"  ```\n{\"x\":1}\n```  ", `{"x":1}`, true},
		{`{"a":`, "", false},
		{"```json\n{\"a\":\n```", "", false},
		{"```{\"x\":1}```", "", false},
	}
	for _, tt := range tests {
		got, err := parseToolInput(json.RawMessage(tt.raw))
		if (err == nil) != tt.ok || string(got) != tt.want {
			t.Errorf("parseToolInput(%q) = %q, %v; want %q, ok=%v", tt.raw, got, err, tt.want, tt.ok)
		}
	}
}

func TestToolShrinkingHistoryMidTurnDoesNotPanic(t *testing.T) {
	truncate := tool.New("truncate", "Drop the history.",
		func(_ context.Context, _ struct{}, c *tool.Call) (tool.Result, error) {
			c.State.SetMessages(nil)
			return tool.Text("dropped"), nil
		}, tool.WithReadOnly())
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "t1", Name: "truncate", Input: map[string]any{}}}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{truncate}})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %s (err %v), want completed", final.Reason, final.Err)
	}
}

// assertToolEventsPaired fails unless every KindToolUse event is followed by
// a KindToolResult for the same call.
func assertToolEventsPaired(t *testing.T, events []Event) {
	t.Helper()
	for i, ev := range events {
		if ev.Kind != KindToolUse {
			continue
		}
		paired := slices.ContainsFunc(events[i+1:], func(later Event) bool {
			return later.Kind == KindToolResult && later.ToolID == ev.ToolID
		})
		if !paired {
			t.Fatalf("KindToolUse %q has no later KindToolResult", ev.ToolID)
		}
	}
}

// assertHistoryPaired fails unless every tool_use in the history is answered
// by a tool_result in the message right after it.
func assertHistoryPaired(t *testing.T, msgs []llm.Message) {
	t.Helper()
	for i, m := range msgs {
		for _, use := range m.ToolUses() {
			answered := i+1 < len(msgs) && slices.ContainsFunc(msgs[i+1].Content, func(b llm.Block) bool {
				return b.Type == llm.BlockToolResult && b.ToolUseID == use.ID
			})
			if !answered {
				t.Fatalf("tool_use %q has no tool_result in the next message", use.ID)
			}
		}
	}
}

// stopperTool stops the run when the call with id stop runs, once ready
// (when non-nil) is closed; any other call succeeds.
func stopperTool(stop string, ready <-chan struct{}) tool.Tool {
	return tool.New("stopper", "Stop the run on one call.",
		func(ctx context.Context, _ echoInput, c *tool.Call) (tool.Result, error) {
			if c.ToolUseID != stop {
				return tool.Text("ok"), nil
			}
			if ready != nil {
				select {
				case <-ready:
				case <-ctx.Done():
					return tool.Result{}, ctx.Err()
				}
			}
			return tool.Result{}, fmt.Errorf("tool: %w", ErrStopRun)
		},
		tool.WithReadOnly(), tool.WithConcurrent())
}

func stopperCall(id string) llmtest.Call {
	return llmtest.Call{ID: id, Name: "stopper", Input: map[string]any{"text": id}}
}

func TestStopRunPairsEveryToolUseEvent(t *testing.T) {
	t.Run("calls waiting for a slot", func(t *testing.T) {
		p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{stopperCall("s"), stopperCall("a"), stopperCall("b")}})
		a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{stopperTool("s", nil)}, MaxConcurrency: 1})

		events, final := collect(t, a.Run(t.Context(), userInput("go")))

		if final.Reason != ReasonHookStopped {
			t.Fatalf("final = %+v, want hook_stopped", final)
		}
		assertToolEventsPaired(t, events)
		assertHistoryPaired(t, final.State.Messages())
	})

	t.Run("call pending on ask", func(t *testing.T) {
		asked := make(chan struct{})
		var once sync.Once
		asker := tool.New("asker", "Needs approval.",
			func(context.Context, echoInput, *tool.Call) (tool.Result, error) { return tool.Text("asked"), nil },
			tool.WithConcurrent(),
			tool.WithPermission(func(context.Context, json.RawMessage, permission.Context) permission.Decision {
				once.Do(func() { close(asked) })
				return permission.AskUser("confirm")
			}))
		p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{
			stopperCall("s"),
			{ID: "q", Name: "asker", Input: map[string]any{"text": "q"}},
		}})
		a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{stopperTool("s", asked), asker}})

		events, final := collect(t, a.Run(t.Context(), userInput("go")))

		if final.Reason != ReasonHookStopped {
			t.Fatalf("final = %+v, want hook_stopped", final)
		}
		if toolUseInput(events, "q") == "" {
			t.Fatal("the pending call q was never announced")
		}
		assertToolEventsPaired(t, events)
		assertHistoryPaired(t, final.State.Messages())
	})
}

func TestHeldCallNotAnnounced(t *testing.T) {
	t.Run("held after an ask", func(t *testing.T) {
		log := newCallLog()
		concurrentWrite := tool.New("write_file", "Write.",
			func(_ context.Context, in writeInput, c *tool.Call) (tool.Result, error) {
				log.add(c)
				return tool.Text("wrote " + in.Text), nil
			},
			tool.WithConcurrent())
		read := tool.New("read", "Read.",
			func(_ context.Context, in echoInput, c *tool.Call) (tool.Result, error) {
				log.add(c)
				return tool.Text("read " + in.Text), nil
			},
			tool.WithReadOnly(), tool.WithConcurrent())

		_, events, _ := interruptedRun(t, Config{Tools: []tool.Tool{concurrentWrite, read}, MaxConcurrency: 1},
			writeCall("w"), readCall("r"))

		if toolUseInput(events, "r") != "" || log.count("r") != 0 {
			t.Fatal("the held read was announced or ran before the interrupt")
		}
	})

	t.Run("held while waiting for a slot", func(t *testing.T) {
		log := newCallLog()
		pause := tool.New("pause", "Interrupt until resumed.",
			func(_ context.Context, _ echoInput, c *tool.Call) (tool.Result, error) {
				log.add(c)
				if c.Resume == nil {
					return tool.Result{}, &Interrupt{}
				}
				return tool.Text("paused"), nil
			},
			tool.WithReadOnly(), tool.WithConcurrent())
		read := tool.New("read", "Read.",
			func(_ context.Context, in echoInput, c *tool.Call) (tool.Result, error) {
				log.add(c)
				return tool.Text("read " + in.Text), nil
			},
			tool.WithReadOnly(), tool.WithConcurrent())

		a, events, final := interruptedRun(t, Config{Tools: []tool.Tool{pause, read}, MaxConcurrency: 1},
			llmtest.Call{ID: "p", Name: "pause", Input: map[string]any{"text": "p"}}, readCall("r"))

		if toolUseInput(events, "r") != "" || log.count("r") != 0 {
			t.Fatal("the read held while waiting for its slot was announced or ran before the interrupt")
		}
		resumed, _ := collect(t, a.Resume(t.Context(), "", final.State, []Decision{{ToolUseID: "p", Type: Approve}}))
		if toolUseInput(resumed, "r") == "" || log.count("r") != 1 {
			t.Fatal("the held read was not announced and run once on resume")
		}
		assertToolEventsPaired(t, resumed)
	})
}

// approvalLog records ToolCall.Approved per tool_use id, as ToolWrappers see
// it.
type approvalLog struct {
	mu       sync.Mutex
	approved map[string]bool
}

func (*approvalLog) Name() string { return "approval_log" }

func (l *approvalLog) WrapTool(ctx context.Context, call *ToolCall, next ToolHandler) (tool.Result, error) {
	l.mu.Lock()
	l.approved[call.ID] = call.Approved
	l.mu.Unlock()
	return next(ctx, call)
}

func TestToolCallApprovedFlag(t *testing.T) {
	newLog := func() *approvalLog { return &approvalLog{approved: map[string]bool{}} }

	t.Run("approver allows", func(t *testing.T) {
		seen := newLog()
		runOneTurn(t, Config{
			Tools:      []tool.Tool{writeTool(newCallLog()), readTool(newCallLog())},
			Middleware: []Middleware{seen},
			Approver: func(context.Context, permission.Request, string) permission.Decision {
				return permission.Allowed()
			},
		}, llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w"), readCall("r")}})
		if !seen.approved["w"] || seen.approved["r"] {
			t.Fatalf("Approved = %v, want true for the approved w only (r is allowed by mode)", seen.approved)
		}
	})

	t.Run("allow rule", func(t *testing.T) {
		seen := newLog()
		runOneTurn(t, Config{
			Tools:      []tool.Tool{writeTool(newCallLog())},
			Middleware: []Middleware{seen},
			Rules:      permission.Rules{Allow: []permission.Rule{{Tool: "write_file"}}},
		}, llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w")}})
		if approved, ran := seen.approved["w"]; !ran || approved {
			t.Fatalf("Approved = %v (ran %v), want false for a rule-allowed call", approved, ran)
		}
	})

	t.Run("resumed approve", func(t *testing.T) {
		seen := newLog()
		log := newCallLog()
		a, _, final := interruptedRun(t, Config{
			Tools:      []tool.Tool{writeTool(log), readTool(log)},
			Middleware: []Middleware{seen},
		}, writeCall("w"), readCall("r"))

		resumeCompleted(t, a, final.State, Decision{ToolUseID: "w", Type: Approve})

		if !seen.approved["w"] || seen.approved["r"] {
			t.Fatalf("Approved = %v, want true for the resumed w only", seen.approved)
		}
	})
}

func TestCheckPermissionDenyRule(t *testing.T) {
	a := mustNew(t, Config{
		Provider: llmtest.New(),
		Tools:    []tool.Tool{writeTool(newCallLog())},
		Rules:    permission.Rules{Deny: []permission.Rule{{Tool: "write_file"}}},
		Approver: func(context.Context, permission.Request, string) permission.Decision {
			t.Fatal("CheckPermission consulted the Approver")
			return permission.Decision{}
		},
	})
	write, _ := a.registry.Get("write_file")
	call := &ToolCall{ID: "w", Name: "write_file", Input: json.RawMessage(`{"text":"x"}`), Tool: write}

	if got := a.CheckPermission(t.Context(), call); got.Behavior != permission.Deny {
		t.Fatalf("CheckPermission = %+v, want deny", got)
	}
}

func TestCheckPermissionAsksWithoutConsultingApprover(t *testing.T) {
	a := mustNew(t, Config{
		Provider: llmtest.New(),
		Tools:    []tool.Tool{writeTool(newCallLog())},
		Approver: func(context.Context, permission.Request, string) permission.Decision {
			t.Fatal("CheckPermission consulted the Approver")
			return permission.Decision{}
		},
	})
	write, _ := a.registry.Get("write_file")
	call := &ToolCall{ID: "w", Name: "write_file", Input: json.RawMessage(`{"text":"x"}`), Tool: write}

	if got := a.CheckPermission(t.Context(), call); got.Behavior != permission.Ask {
		t.Fatalf("CheckPermission = %+v, want ask in the default mode", got)
	}
}
