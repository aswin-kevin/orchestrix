package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// writeFile creates path with content, making its parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// systemText joins the system segments of a request.
func systemText(req llm.Request) string { return strings.Join(req.System, "\n") }

// toolNames lists the tools offered by a request.
func toolNames(req llm.Request) []string {
	var names []string
	for _, ts := range req.Tools {
		names = append(names, ts.Name)
	}
	return names
}

// toolResult returns the text of the result of call id in the requests the
// provider saw, and whether it was an error.
func toolResult(t *testing.T, p *llmtest.Scripted, id string) (text string, isError bool) {
	t.Helper()
	for _, req := range p.Requests {
		for _, msg := range req.Messages {
			for _, b := range msg.Content {
				if b.Type != llm.BlockToolResult || b.ToolUseID != id {
					continue
				}
				var sb strings.Builder
				for _, c := range b.Content {
					sb.WriteString(c.Text)
				}
				return sb.String(), b.IsError
			}
		}
	}
	t.Fatalf("no result for call %s", id)
	return "", false
}

// textTurn scripts a final answer without tool calls.
func textTurn() *llmtest.Scripted { return llmtest.New(llmtest.Turn{Text: "ok"}) }

func TestSkillsFlagLoadsCatalog(t *testing.T) {
	skillsDir := t.TempDir()
	writeFile(t, filepath.Join(skillsDir, "release-notes", "SKILL.md"),
		"---\nname: release-notes\ndescription: Writes release notes from the git log.\n---\nSteps.\n")
	writeFile(t, filepath.Join(skillsDir, "Bad_Name", "SKILL.md"),
		"---\nname: Bad_Name\ndescription: Invalid name.\n---\n")
	provider := textTurn()

	code, _, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(),
		"-skills", skillsDir, "-p", "hi")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if sys := systemText(provider.Requests[0]); !strings.Contains(sys, "release-notes") {
		t.Errorf("system prompt does not list the skill: %q", sys)
	}
	if !strings.Contains(stderr, "Bad_Name") {
		t.Errorf("stderr = %q, want a warning about the invalid skill", stderr)
	}
}

func TestSkillsFlagUnreadableDirIsUsageError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	code, _, _ := invoke(t, "", textTurn(), "-model", "openai:test", "-dir", t.TempDir(), "-skills", missing, "-p", "hi")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
}

func TestMemoryLoadedByDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Always run the linter.")
	writeFile(t, filepath.Join(dir, "notes", "extra.md"), "Prefer table tests.")
	tests := []struct {
		name      string
		args      []string
		want, not []string
	}{
		{"default files", nil, []string{"Always run the linter."}, []string{"Prefer table tests."}},
		{"relative -memory path", []string{"-memory", "notes/extra.md"},
			[]string{"Always run the linter.", "Prefer table tests."}, nil},
		{"-no-memory", []string{"-no-memory"}, nil, []string{"Always run the linter."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := textTurn()
			args := append([]string{"-model", "openai:test", "-dir", dir, "-p", "hi"}, tt.args...)

			code, _, stderr := invoke(t, "", provider, args...)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
			}
			sys := systemText(provider.Requests[0])
			for _, w := range tt.want {
				if !strings.Contains(sys, w) {
					t.Errorf("system prompt lacks %q", w)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(sys, n) {
					t.Errorf("system prompt contains %q", n)
				}
			}
		})
	}
}

func TestMissingMemoryFileWarnsOnlyWhenExplicit(t *testing.T) {
	dir := t.TempDir() // holds no AGENTS.md

	code, _, stderr := invoke(t, "", textTurn(), "-model", "openai:test", "-dir", dir, "-memory", "notes/missing.md", "-p", "hi")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "/notes/missing.md") {
		t.Errorf("stderr = %q, want a warning naming the missing -memory file", stderr)
	}
	if strings.Contains(stderr, "AGENTS.md") {
		t.Errorf("stderr = %q, want no warning for the missing default file", stderr)
	}
}

func TestMemoryStoreFlagAddsTools(t *testing.T) {
	provider := textTurn()

	code, _, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-memory-store", "-p", "hi")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if names := toolNames(provider.Requests[0]); !slices.Contains(names, "record_memory") {
		t.Errorf("tools = %v, want record_memory", names)
	}
}

func TestMemoryFlagConflictsAreUsageErrors(t *testing.T) {
	for _, extra := range []string{"-memory-store", "-memory=x.md"} {
		t.Run(extra, func(t *testing.T) {
			code, _, _ := invoke(t, "", textTurn(), "-model", "openai:test", "-no-memory", extra, "-p", "hi")
			if code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
		})
	}
}

func TestMCPOptionalServerFailureWarns(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "mcp.json")
	writeFile(t, cfg, `{"mcpServers": {"gone": {"command": "`+filepath.ToSlash(filepath.Join(t.TempDir(), "no-such-server"))+`", "optional": true}}}`)

	code, _, stderr := invoke(t, "", textTurn(), "-model", "openai:test", "-dir", t.TempDir(), "-mcp", cfg, "-p", "hi")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "gone") {
		t.Errorf("stderr = %q, want a warning naming the server", stderr)
	}
}

func TestMCPBadConfigIsUsageError(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "mcp.json")
	writeFile(t, cfg, `{"mcpServers": {"x": {}}}`)
	code, _, _ := invoke(t, "", textTurn(), "-model", "openai:test", "-dir", t.TempDir(), "-mcp", cfg, "-p", "hi")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
}

// askTurns scripts an ask_user call with options A and B, then an answer.
func askTurns() *llmtest.Scripted {
	return llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "q1", Name: "ask_user", Input: map[string]any{
			"questions": []map[string]any{{
				"question": "Which database?",
				"options":  []map[string]string{{"label": "Alpha"}, {"label": "Beta"}},
			}},
		}}}},
		llmtest.Turn{Text: "thanks"},
	)
}

func TestAskUserPromptsTerminal(t *testing.T) {
	provider := askTurns()

	code, _, stderr := invoke(t, "2\n", provider, "-model", "openai:test", "-dir", t.TempDir(), "-p", "choose")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	for _, want := range []string{"Which database?", "1. Alpha", "2. Beta"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	text, isErr := toolResult(t, provider, "q1")
	if isErr || !strings.Contains(text, "Beta") || strings.Contains(text, "Alpha") {
		t.Errorf("ask_user result = %q (error %v), want the answer Beta", text, isErr)
	}
}

func TestAskUserBlankAnswer(t *testing.T) {
	provider := askTurns()

	code, _, stderr := invoke(t, "\n", provider, "-model", "openai:test", "-dir", t.TempDir(), "-p", "choose")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if text, isErr := toolResult(t, provider, "q1"); isErr || !strings.Contains(text, "(no answer)") {
		t.Errorf("ask_user result = %q (error %v), want (no answer)", text, isErr)
	}
}

func TestAskUserEndOfInputIsToolError(t *testing.T) {
	provider := askTurns()

	code, _, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-p", "choose")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if _, isErr := toolResult(t, provider, "q1"); !isErr {
		t.Error("ask_user result is not an error after end of input")
	}
}

// planTurns scripts exit_plan_mode with plan, then a write_file, then an
// answer.
func planTurns(plan string) *llmtest.Scripted {
	return llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "p1", Name: "exit_plan_mode", Input: map[string]string{"plan": plan}}}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w1", Name: "write_file", Input: map[string]string{"path": "/out.txt", "content": "hi"}}}},
		llmtest.Turn{Text: "done"},
	)
}

func TestPlanApprovalPrintsPlan(t *testing.T) {
	dir := t.TempDir()
	provider := planTurns("step one\n\tstep two")

	// The plan is approved, then the write is approved: after approval the
	// run is in the default mode (it started in plan mode, which recorded no
	// earlier mode), where write_file asks.
	code, stdout, stderr := invoke(t, "y\ny\n", provider, "-model", "openai:test", "-dir", dir, "-mode", "plan", "-p", "plan it")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "step one\n\tstep two") {
		t.Errorf("stderr = %q, want the full plan with its layout", stderr)
	}
	if strings.Contains(stdout, "step one") {
		t.Errorf("stdout = %q, want the plan on stderr only", stdout)
	}
	if !strings.Contains(stderr, "Approve plan? [y/N] ") {
		t.Errorf("stderr = %q, want the plan approval prompt", stderr)
	}
	text, _ := toolResult(t, provider, "p1")
	if !strings.Contains(text, "Permission mode is now default") {
		t.Errorf("exit_plan_mode result = %q, want the default mode after approval", text)
	}
	if !strings.Contains(stderr, "Allow write_file /out.txt? [y/N/a] ") {
		t.Errorf("stderr = %q, want write_file to ask in the default mode", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Errorf("write after approval failed: %v", err)
	}
}

func TestPlanRejectionFeedbackReachesModel(t *testing.T) {
	dir := t.TempDir()
	provider := planTurns("step one")

	code, _, stderr := invoke(t, "n\nwrite the tests first\n", provider, "-model", "openai:test", "-dir", dir, "-mode", "plan", "-p", "plan it")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	text, _ := toolResult(t, provider, "p1")
	if !strings.Contains(text, "did not approve") || !strings.Contains(text, "write the tests first") {
		t.Errorf("exit_plan_mode result = %q, want the rejection with the feedback", text)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Error("file written although the plan was rejected and plan mode stayed on")
	}
}

func TestWebSearchNeedsKey(t *testing.T) {
	t.Setenv(braveKeyEnv, "")
	t.Setenv(tavilyKeyEnv, "")
	tests := []struct {
		name, env string
		args      []string
	}{
		{"brave without key", braveKeyEnv, []string{"-web", "-search", "brave"}},
		{"tavily without key", tavilyKeyEnv, []string{"-web", "-search", "tavily"}},
		{"unknown backend", "", []string{"-web", "-search", "bogus"}},
		{"search without web", "", []string{"-search", "ddg"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-model", "openai:test", "-dir", t.TempDir(), "-p", "hi"}, tt.args...)
			code, _, stderr := invoke(t, "", textTurn(), args...)
			if code != exitUsage {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, exitUsage, stderr)
			}
			if tt.env != "" && !strings.Contains(stderr, tt.env) {
				t.Errorf("stderr = %q, want it to name %s", stderr, tt.env)
			}
		})
	}
}

func TestWebFlagWiring(t *testing.T) {
	const key = "secret-key-value"
	t.Setenv(braveKeyEnv, key)
	for _, args := range [][]string{{"-web"}, {"-web", "-search", "brave"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			provider := textTurn()
			code, stdout, stderr := invoke(t, "", provider,
				append([]string{"-model", "openai:test", "-dir", t.TempDir(), "-p", "hi"}, args...)...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
			}
			names := toolNames(provider.Requests[0])
			if !slices.Contains(names, "web_fetch") || !slices.Contains(names, "web_search") {
				t.Errorf("tools = %v, want web_fetch and web_search", names)
			}
			if strings.Contains(stdout+stderr, key) {
				t.Error("output echoes the API key")
			}
		})
	}
}

func TestWebToolsAskForApproval(t *testing.T) {
	provider := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "f1", Name: "web_fetch", Input: map[string]string{"url": "https://example.invalid/"}}}},
		llmtest.Turn{Text: "done"},
	)

	code, _, stderr := invoke(t, "n\n", provider, "-model", "openai:test", "-dir", t.TempDir(), "-web", "-p", "fetch")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "Allow web_fetch") {
		t.Errorf("stderr = %q, want an approval prompt for web_fetch", stderr)
	}
	if text, _ := toolResult(t, provider, "f1"); !strings.Contains(text, deniedByUser) {
		t.Errorf("web_fetch result = %q, want it denied", text)
	}
}

func TestSkillsLoadedOnce(t *testing.T) {
	skillsDir := t.TempDir()
	writeFile(t, filepath.Join(skillsDir, "release-notes", "SKILL.md"),
		"---\nname: release-notes\ndescription: Writes release notes from the git log.\n---\nSteps.\n")
	opt, err := skillsOption([]string{skillsDir}, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	// What orx warned about must be what the agent gets: a change on disk
	// after the warnings were printed must not reach the agent.
	if err := os.RemoveAll(filepath.Join(skillsDir, "release-notes")); err != nil {
		t.Fatal(err)
	}
	provider := textTurn()
	a, err := orchestrix.NewWithProvider(provider, opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range a.Run(t.Context(), agent.Input{Messages: []llm.Message{llm.UserText("hi")}}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if sys := systemText(provider.Requests[0]); !strings.Contains(sys, "release-notes") {
		t.Errorf("the agent read the skills directory again: the skill is missing from %q", sys)
	}
}

func TestAsyncFlag(t *testing.T) {
	provider := textTurn()

	code, _, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-async", "-p", "hi")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if names := toolNames(provider.Requests[0]); !slices.Contains(names, "start_task") {
		t.Errorf("tools = %v, want start_task", names)
	}
}

func TestCompressFlag(t *testing.T) {
	provider := textTurn()

	code, _, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-compress", "-p", "hi")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if names := toolNames(provider.Requests[0]); !slices.Contains(names, "compress_context") {
		t.Errorf("tools = %v, want compress_context", names)
	}
}
