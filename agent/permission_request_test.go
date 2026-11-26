package agent_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/permission"
)

func TestPermissionRequestMatchesLoop(t *testing.T) {
	a, err := agent.New(agent.Config{
		Provider:   llmtest.New(),
		Middleware: []agent.Middleware{mustFS(t, filesystem.Config{Backend: testutil.Memory(t)})},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := json.RawMessage(`{"path":"/a.txt","content":"x"}`)

	req := a.PermissionRequest(t.Context(), &agent.ToolCall{Name: filesystem.ToolWriteFile, Input: in})

	want := []permission.PathAccess{{Path: "/a.txt", Op: permission.OpWrite}}
	if req.Tool != filesystem.ToolWriteFile || string(req.Input) != string(in) {
		t.Fatalf("request = %+v, want write_file with the call's input", req)
	}
	if !slices.Equal(req.Paths, want) || !req.Edit || req.ReadOnly {
		t.Fatalf("request Paths %v Edit %v ReadOnly %v, want %v, edit, not read-only", req.Paths, req.Edit, req.ReadOnly, want)
	}
}

func TestPermissionRequestUnknownTool(t *testing.T) {
	a, err := agent.New(agent.Config{Provider: llmtest.New()})
	if err != nil {
		t.Fatal(err)
	}
	in := json.RawMessage(`{}`)

	req := a.PermissionRequest(t.Context(), &agent.ToolCall{Name: "nope", Input: in})

	if req.Tool != "nope" || string(req.Input) != "{}" || req.ReadOnly || req.Paths != nil {
		t.Fatalf("request = %+v, want only Tool and Input", req)
	}
}
