package orchestrix_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// secretWorkspace holds one file under a read-denied directory and one
// visible file, both containing the word the searches look for.
func secretWorkspace(t *testing.T) *memory.Backend {
	t.Helper()
	mem := testutil.Memory(t)
	ctx := context.Background()
	for path, content := range map[string]string{
		"/secrets/key.txt": "TOKEN=hidden\n",
		"/notes/a.txt":     "TOKEN=public\n",
	} {
		if err := mem.Write(ctx, path, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	return mem
}

// toolResultText runs one scripted tool call followed by a closing answer
// and returns the text of the call's tool_result.
func toolResultText(t *testing.T, call llmtest.Call, opts ...orchestrix.Option) string {
	t.Helper()
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{call}}, llmtest.Turn{Text: "done"})
	a, err := orchestrix.NewWithProvider(p, opts...)
	if err != nil {
		t.Fatal(err)
	}
	final := finalOf(t, a, orchestrix.Text("go"))
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %s (err %v), want completed", final.Reason, final.Err)
	}
	return resultOf(t, final.State, call.ID)
}

// resultOf returns the flattened text of the tool_result answering id.
func resultOf(t *testing.T, s *state.State, id string) string {
	t.Helper()
	for _, m := range s.Messages() {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && b.ToolUseID == id {
				return llm.Message{Content: b.Content}.Text()
			}
		}
	}
	t.Fatalf("no tool_result for %s in history", id)
	return ""
}

func TestSearchToolsHideReadDeniedPaths(t *testing.T) {
	tests := []struct {
		name string
		call llmtest.Call
	}{
		{"grep content", llmtest.Call{ID: "c1", Name: "grep", Input: map[string]any{"pattern": "TOKEN", "mode": "content"}}},
		{"grep files", llmtest.Call{ID: "c1", Name: "grep", Input: map[string]any{"pattern": "TOKEN"}}},
		{"grep count", llmtest.Call{ID: "c1", Name: "grep", Input: map[string]any{"pattern": "TOKEN", "mode": "count"}}},
		{"glob", llmtest.Call{ID: "c1", Name: "glob", Input: map[string]any{"pattern": "**/*"}}},
		{"ls", llmtest.Call{ID: "c1", Name: "ls", Input: map[string]any{"path": "/"}}},
	}
	for _, rule := range []string{"deny", "ask"} {
		for _, tt := range tests {
			t.Run(rule+"/"+tt.name, func(t *testing.T) {
				opt := orchestrix.WithDeny("read(/secrets/**)")
				if rule == "ask" {
					opt = orchestrix.WithAsk("read(/secrets/**)")
				}
				got := toolResultText(t, tt.call, orchestrix.WithBackend(secretWorkspace(t)), opt)
				if strings.Contains(got, "secrets") || strings.Contains(got, "hidden") {
					t.Fatalf("result reveals the %s-gated directory:\n%s", rule, got)
				}
				if !strings.Contains(got, "/notes") {
					t.Fatalf("result lost the visible file:\n%s", got)
				}
			})
		}
	}
}

func TestResumeEditIsCheckedAgainstDenyRules(t *testing.T) {
	mem := testutil.Memory(t)
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{
			ID: "w", Name: "write_file", Input: map[string]any{"path": "/notes/x.txt", "content": "x"},
		}}},
		llmtest.Turn{Text: "done"},
	)
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithBackend(mem), orchestrix.WithDeny("write(/secrets/**)"))
	if err != nil {
		t.Fatal(err)
	}
	final := finalOf(t, a, orchestrix.Text("go"))
	if final.Reason != agent.ReasonInterrupted {
		t.Fatalf("final = %s (err %v), want interrupted", final.Reason, final.Err)
	}

	edit := agent.Decision{ToolUseID: "w", Type: agent.Edit, Input: []byte(`{"path":"/secrets/k","content":"leak"}`)}
	var resumed *agent.Final
	for ev := range a.Resume(context.Background(), "", final.State, []agent.Decision{edit}) {
		resumed = ev.Final
	}
	if resumed == nil || resumed.Reason != agent.ReasonCompleted {
		t.Fatalf("resumed final = %+v, want completed", resumed)
	}
	if got := resultOf(t, resumed.State, "w"); !strings.HasPrefix(got, "Permission denied") {
		t.Fatalf("edited call result = %q, want a permission denial", got)
	}
	if _, err := mem.Read(context.Background(), "/secrets/k", 0, 1); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("denied file was written (read err = %v)", err)
	}
}

func TestAskRuleWinsOverToolSelfAllow(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{
			ID: "t", Name: "write_todos", Input: map[string]any{"todos": []map[string]string{{"content": "a", "status": "pending"}}},
		}}},
		llmtest.Turn{Text: "done"},
	)
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithAsk("write_todos"))
	if err != nil {
		t.Fatal(err)
	}
	final := finalOf(t, a, orchestrix.Text("go"))
	if final.Reason != agent.ReasonInterrupted {
		t.Fatalf("final = %s (err %v), want interrupted by the ask rule", final.Reason, final.Err)
	}
}

func TestWriteTodosAllowedInNonInteractiveModes(t *testing.T) {
	for _, mode := range []permission.Mode{permission.ModePlan, permission.ModeDontAsk} {
		t.Run(string(mode), func(t *testing.T) {
			call := llmtest.Call{
				ID: "t", Name: "write_todos", Input: map[string]any{"todos": []map[string]string{{"content": "a", "status": "pending"}}},
			}
			p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{call}}, llmtest.Turn{Text: "done"})
			a, err := orchestrix.NewWithProvider(p, orchestrix.WithPermissionMode(mode))
			if err != nil {
				t.Fatal(err)
			}
			final := finalOf(t, a, orchestrix.Text("go"))
			if got := planning.Todos(final.State); len(got) != 1 {
				t.Fatalf("todos = %+v (result %q), want the list written", got, resultOf(t, final.State, "t"))
			}
		})
	}
}

func TestToolSelfDenyWinsInBypassMode(t *testing.T) {
	ran := false
	guarded := tool.New("guarded", "Refuses to run.",
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
			ran = true
			return tool.Text("ran"), nil
		},
		tool.WithPermission(func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Denied("guarded refuses")
		}))

	got := toolResultText(t, llmtest.Call{ID: "g", Name: "guarded"},
		orchestrix.WithTools(guarded), orchestrix.WithPermissionMode(permission.ModeBypass))
	if ran || got != "Permission denied: guarded refuses" {
		t.Fatalf("ran = %v, result = %q; want the tool's own denial to stand in bypass mode", ran, got)
	}
}
