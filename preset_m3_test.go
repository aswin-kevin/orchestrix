package orchestrix_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
	"github.com/aswin-kevin/orchestrix/middleware/web"
	"github.com/aswin-kevin/orchestrix/tool"
)

const pdfBody = "Extract the text before reading a PDF."

// testSkills defines the skills "pdf" and "sheets".
func testSkills() skills.Config {
	return skills.Config{Skills: []skills.Skill{
		{Name: "pdf", Description: "Work with PDF files.", Body: pdfBody},
		{Name: "sheets", Description: "Work with spreadsheets.", Body: "Use the CSV export."},
	}}
}

// connectMCP connects an in-process MCP server offering one tool, "lookup",
// and closes the middleware when the test ends.
func connectMCP(t *testing.T, gated bool) *mcp.Middleware {
	t.Helper()
	srv := mcp.NewServer(mcp.Implementation{Name: "docs", Version: "1"}, "")
	srv.AddTool(mcp.ToolInfo{Name: "lookup", Description: "Looks a term up."},
		func(context.Context, json.RawMessage) (*mcp.CallToolResult, error) {
			return mcp.TextResult("found"), nil
		})
	m, err := mcp.Connect(context.Background(), []mcp.ServerConfig{{Name: "docs", Transport: mcp.InProcess(srv), Gated: gated}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func TestStackOrderM3(t *testing.T) {
	tm := tasks.New(tasks.Config{})
	defer tm.Shutdown()
	a, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.WithShell(t.TempDir(), shell.Options{}),
		orchestrix.WithTasks(tm),
		orchestrix.WithHooks(hooks.Config{}),
		orchestrix.WithInterruptOn("write_file"),
		orchestrix.WithMemory(memory.Config{Files: memory.DefaultFiles}),
		orchestrix.WithSkills(testSkills()),
		orchestrix.WithPlanMode(planmode.Config{}),
		orchestrix.WithDeferredTools(deferred.Config{}),
		orchestrix.WithMCP(connectMCP(t, false)),
		orchestrix.WithWeb(web.Config{}),
		orchestrix.WithAskUser(askuser.Config{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"summarize", "cache", "hooks", "hitl", "memory", "skills", "planmode",
		"filesystem", "planning", "subagents", "deferred", "mcp", "web", "askuser", "tasks"}
	if got := stackNames(a); !slices.Equal(got, want) {
		t.Errorf("stack = %v, want %v", got, want)
	}
}

func TestMCPWithoutHiddenToolsAddsNoDeferred(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithMCP(connectMCP(t, false)))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(stackNames(a), "deferred") {
		t.Errorf("stack %v has deferred, though no tool needs it", stackNames(a))
	}
}

func TestSkillsWithToolsAddsDeferred(t *testing.T) {
	cfg := testSkills()
	cfg.Tools = []tool.Tool{noopTool("pdf_render")}
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithSkills(cfg),
		orchestrix.WithMCP(connectMCP(t, false)),
		orchestrix.WithWeb(web.Config{}),
		orchestrix.WithAskUser(askuser.Config{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))
	names := toolNames(p)
	if slices.Contains(names, "pdf_render") {
		t.Errorf("tools %v show the gated skill tool pdf_render", names)
	}
	if !slices.Contains(names, deferred.ToolSearch) {
		t.Errorf("tools %v lack %s", names, deferred.ToolSearch)
	}
	want := []string{"summarize", "cache", "skills", "filesystem", "planning", "subagents", "deferred", "mcp", "web", "askuser"}
	if got := stackNames(a); !slices.Equal(got, want) {
		t.Errorf("stack = %v, want %v (auto-added deferred after subagents)", got, want)
	}
}

func TestFailedSkillsDoNotCascadeToSubagents(t *testing.T) {
	cfg := testSkills()
	cfg.Pinned = []string{"nope"}
	_, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.WithSkills(cfg),
		orchestrix.WithSubagents(subagents.Subagent{
			Name: "pdf-helper", Description: "Handles PDFs.", SystemPrompt: "You handle PDFs.",
			Skills: []string{"pdf"},
		}),
	)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want the unknown pinned skill reported", err)
	}
	if strings.Contains(err.Error(), "skills middleware") {
		t.Errorf("err = %v also blames a missing skills middleware; the WithSkills error is the cause", err)
	}
}

func TestMCPHiddenToolsAddsDeferred(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithMCP(connectMCP(t, true)))
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))
	names := toolNames(p)
	if !slices.Contains(names, deferred.ToolSearch) {
		t.Errorf("tools %v lack %s", names, deferred.ToolSearch)
	}
	if lookup := mcp.ToolName("docs", "lookup"); slices.Contains(names, lookup) {
		t.Errorf("tools %v show the gated %s", names, lookup)
	}
}

func TestHiddenWithToolsAddsDeferred(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool tool.Tool
	}{
		{"gated", deferred.Gate(noopTool("secret"))},
		{"deferred", deferred.Defer(noopTool("secret"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := llmtest.New(llmtest.Turn{Text: "ok"})
			a, err := orchestrix.NewWithProvider(p, orchestrix.WithTools(tc.tool))
			if err != nil {
				t.Fatal(err)
			}
			finalOf(t, a, orchestrix.Text("hi"))
			if names := toolNames(p); slices.Contains(names, "secret") {
				t.Errorf("tools %v show the %s tool's schema", names, tc.name)
			}
			if !slices.Contains(stackNames(a), "deferred") {
				t.Errorf("stack %v lacks deferred", stackNames(a))
			}
		})
	}
}

func TestExplicitDeferredConfigKept(t *testing.T) {
	cfg := testSkills()
	cfg.Tools = []tool.Tool{noopTool("pdf_render")}
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithSkills(cfg),
		orchestrix.WithDeferredTools(deferred.Config{Tools: []string{"write_file"}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))
	if n := strings.Count(strings.Join(stackNames(a), ","), "deferred"); n != 1 {
		t.Errorf("stack %v has %d deferred middleware, want 1", stackNames(a), n)
	}
	if names := toolNames(p); slices.Contains(names, "write_file") {
		t.Errorf("tools %v show write_file, which the explicit config defers", names)
	}
}

func TestWithWebBadProxyFailsNew(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithWeb(web.Config{ProxyURL: "ftp://proxy:21"}))
	if err == nil || !strings.Contains(err.Error(), "WithWeb") {
		t.Errorf("err = %v, want a WithWeb error", err)
	}
}

func TestWithSkillsUnknownPinnedFailsNew(t *testing.T) {
	cfg := testSkills()
	cfg.Pinned = []string{"nope"}
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithSkills(cfg))
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v, want the unknown pinned skill reported", err)
	}
}

func TestWithMemoryRelativePathFailsNew(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithMemory(memory.Config{Files: []string{"AGENTS.md"}}))
	if err == nil || !strings.Contains(err.Error(), "WithMemory") {
		t.Errorf("err = %v, want a WithMemory error", err)
	}
}

func TestWithMCPNilFailsNew(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithMCP(nil))
	if err == nil || !strings.Contains(err.Error(), "WithMCP") {
		t.Errorf("err = %v, want a WithMCP error", err)
	}
}

func TestM3ConstructorErrorsAreJoined(t *testing.T) {
	cfg := testSkills()
	cfg.Pinned = []string{"nope"}
	_, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.WithMemory(memory.Config{Files: []string{"AGENTS.md"}}),
		orchestrix.WithSkills(cfg),
		orchestrix.WithWeb(web.Config{ProxyURL: "ftp://proxy:21"}),
	)
	for _, want := range []string{"WithMemory", "WithSkills", "WithWeb"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %s", err, want)
		}
	}
}

// delegate scripts a parent that starts subagent typ, the child answering
// "child done", and the parent finishing.
func delegate(typ string) *llmtest.Scripted {
	return llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "t1", Name: subagents.ToolTask,
			Input: subagents.TaskInput{Description: "do it", SubagentType: typ}}}},
		llmtest.Turn{Text: "child done"},
		llmtest.Turn{Text: "parent done"},
	)
}

// requestText is every system segment and message text of req.
func requestText(req llm.Request) string {
	var b strings.Builder
	for _, seg := range req.System {
		b.WriteString(seg + "\n")
	}
	for _, msg := range req.Messages {
		b.WriteString(msg.Text() + "\n")
	}
	return b.String()
}

func TestSubagentSkillsThroughPreset(t *testing.T) {
	p := delegate("pdf-helper")
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithSkills(testSkills()),
		orchestrix.WithSubagents(subagents.Subagent{
			Name: "pdf-helper", Description: "Handles PDFs.", SystemPrompt: "You handle PDFs.",
			Skills: []string{"pdf"},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if final := finalOf(t, a, orchestrix.Text("go")); final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %s (%v), want completed", final.Reason, final.Err)
	}
	parent, child := requestText(p.Requests[0]), requestText(p.Requests[1])
	if !strings.Contains(parent, "- pdf:") || !strings.Contains(parent, "- sheets:") {
		t.Errorf("parent catalog should list pdf and sheets:\n%s", parent)
	}
	if !strings.Contains(child, "- pdf:") || strings.Contains(child, "- sheets:") {
		t.Errorf("child catalog should list pdf only:\n%s", child)
	}
}

func TestSubagentPinnedSkillInjectedOnce(t *testing.T) {
	cfg := testSkills()
	cfg.Pinned = []string{"pdf"} // pinned for the parent too: still once in the child
	p := delegate("pdf-helper")
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithSkills(cfg),
		orchestrix.WithSubagents(subagents.Subagent{
			Name: "pdf-helper", Description: "Handles PDFs.", SystemPrompt: "You handle PDFs.",
			PinnedSkills: []string{"pdf"},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("go"))
	child := requestText(p.Requests[1])
	if n := strings.Count(child, pdfBody); n != 1 {
		t.Errorf("child request holds the pdf instructions %d times, want 1:\n%s", n, child)
	}
}

func TestSubagentUnknownSkillFailsNew(t *testing.T) {
	for name, spec := range map[string]subagents.Subagent{
		"skills": {Skills: []string{"nope"}},
		"pinned": {PinnedSkills: []string{"nope"}},
	} {
		spec.Name, spec.Description, spec.SystemPrompt = "helper", "Helps.", "You help."
		_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithSkills(testSkills()), orchestrix.WithSubagents(spec))
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Errorf("%s: err = %v, want the unknown skill reported", name, err)
		}
	}
}

func TestSubagentSkillsWithoutSkillsMiddlewareFailNew(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithSubagents(subagents.Subagent{
		Name: "helper", Description: "Helps.", SystemPrompt: "You help.", Skills: []string{"pdf"},
	}))
	if err == nil || !strings.Contains(err.Error(), "skills middleware") {
		t.Errorf("err = %v, want the missing skills middleware reported", err)
	}
}

func noopTool(name string) tool.Tool {
	return tool.New(name, "Does nothing.", func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
		return tool.Text("ok"), nil
	})
}
