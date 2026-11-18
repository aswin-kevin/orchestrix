package agent

import (
	"context"
	"encoding/json"
	"strings"
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
	_, final := collect(t, mustNew(t, cfg).Run(context.Background(), userInput("go")))
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
		tool.WithSubject(func(json.RawMessage) string { return "subject" }),
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

	if !got.Edit || got.ReadOnly || got.Subject != "subject" || len(got.Paths) != 1 || got.Paths[0] != paths[0] {
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

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

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

	events, _ := collect(t, a.Run(context.Background(), userInput("go")))

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
	cases := []struct {
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
	for _, c := range cases {
		got, err := parseToolInput(json.RawMessage(c.raw))
		if (err == nil) != c.ok || string(got) != c.want {
			t.Errorf("parseToolInput(%q) = %q, %v; want %q, ok=%v", c.raw, got, err, c.want, c.ok)
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

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %s (err %v), want completed", final.Reason, final.Err)
	}
}
