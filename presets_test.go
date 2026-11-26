package orchestrix_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	membackend "github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/cache"
	"github.com/aswin-kevin/orchestrix/middleware/compress"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/middleware/hitl"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/jobs"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
	"github.com/aswin-kevin/orchestrix/middleware/profiles"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/middleware/web"
	"github.com/aswin-kevin/orchestrix/permission"
)

func stackNames(a *agent.Agent) []string {
	var names []string
	for _, m := range a.Config().Middleware {
		names = append(names, m.Name())
	}
	return names
}

func TestFullStackOrder(t *testing.T) {
	sh := t.TempDir()
	tm := mustJobs(t)
	a, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.WithShell(sh, shell.Config{}),
		orchestrix.WithJobs(tm),
		orchestrix.WithHooks(hooks.Config{}),
		orchestrix.WithInterruptOn("write_file"),
		orchestrix.WithCheckpointer(mustFileCheckpointer(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, a)
	want := []string{"summarize", "cache", "hooks", "hitl", "filesystem", "planning", "subagents", "jobs"}
	if got := stackNames(a); !slices.Equal(got, want) {
		t.Errorf("full stack = %v, want %v", got, want)
	}
}

// TestStackOrder pins the order New's doc gives, by the middleware's Name
// constants: the default stack, then the optional middleware in two groups.
func TestStackOrder(t *testing.T) {
	tests := []struct {
		name string
		opts func(t *testing.T) []orchestrix.Option
		want []string
	}{
		{
			name: "base",
			opts: func(*testing.T) []orchestrix.Option { return nil },
			want: []string{summarize.Name, cache.Name, filesystem.Name, planning.Name, subagents.Name},
		},
		{
			name: "tools",
			opts: func(t *testing.T) []orchestrix.Option {
				return []orchestrix.Option{
					orchestrix.WithShell(t.TempDir(), shell.Config{}),
					orchestrix.WithJobs(mustJobs(t)),
					orchestrix.WithHooks(hooks.Config{}),
					orchestrix.WithInterruptOn("write_file"),
					orchestrix.WithMemory(memory.Config{Files: memory.DefaultFiles()}),
					orchestrix.WithSkills(testSkills()),
					orchestrix.WithPlanMode(planmode.Config{}),
					orchestrix.WithDeferredTools(deferred.Config{}),
					orchestrix.WithMCP(connectMCP(t, false)),
					orchestrix.WithWeb(web.Config{}),
					orchestrix.WithAskUser(askuser.Config{}),
				}
			},
			want: []string{summarize.Name, cache.Name, hooks.Name, hitl.Name, memory.Name, skills.Name,
				planmode.Name, filesystem.Name, planning.Name, subagents.Name, deferred.Name, mcp.Name,
				web.Name, askuser.Name, jobs.Name},
		},
		{
			name: "scale",
			opts: func(t *testing.T) []orchestrix.Option {
				am, err := async.New(async.Config{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = am.Close(context.Background()) })
				return []orchestrix.Option{
					orchestrix.WithShell(t.TempDir(), shell.Config{}),
					orchestrix.WithJobs(mustJobs(t)),
					orchestrix.WithHooks(hooks.Config{}),
					orchestrix.WithInterruptOn("write_file"),
					orchestrix.WithMemory(memory.Config{Files: memory.DefaultFiles()}),
					orchestrix.WithSkills(testSkills()),
					orchestrix.WithPlanMode(planmode.Config{}),
					orchestrix.WithDeferredTools(deferred.Config{}),
					orchestrix.WithMCP(connectMCP(t, false)),
					orchestrix.WithWeb(web.Config{}),
					orchestrix.WithAskUser(askuser.Config{}),
					orchestrix.WithAsync(am),
					orchestrix.WithCompress(compress.Config{}),
					orchestrix.WithRubric(passRubric()),
					orchestrix.WithProfiles(profiles.Config{}),
					orchestrix.WithMount("/kv", kvBackend(t)),
				}
			},
			want: []string{summarize.Name, compress.Name, cache.Name, hooks.Name, hitl.Name, rubric.Name,
				memory.Name, skills.Name, planmode.Name, filesystem.Name, planning.Name, subagents.Name,
				async.Name, deferred.Name, mcp.Name, web.Name, askuser.Name, jobs.Name, profiles.Name},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := orchestrix.NewWithProvider(llmtest.New(), tt.opts(t)...)
			if err != nil {
				t.Fatal(err)
			}
			closeAtEnd(t, a)
			got := stackNames(a)
			if !slices.Equal(got, tt.want) {
				t.Errorf("stack = %v, want %v", got, tt.want)
			}
			if tt.name != "scale" {
				return
			}
			// Every slot is on here, so each built middleware's Name must
			// be its slot's name: WithMiddleware and WithoutMiddleware
			// match by it.
			if slots := orchestrix.PresetStackNames(); !slices.Equal(got, slots) {
				t.Errorf("built names = %v, slot names = %v", got, slots)
			}
		})
	}
}

func TestWithoutDefaultMiddleware(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithoutMiddleware("subagents", "summarize", "cache"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"filesystem", "planning"}
	if got := stackNames(a); !slices.Equal(got, want) {
		t.Errorf("stack = %v, want %v", got, want)
	}
}

func mustFileCheckpointer(t *testing.T) *file.Checkpointer {
	t.Helper()
	c, err := file.New(t.TempDir(), file.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaultWorkspaceSurvivesCheckpoint(t *testing.T) {
	dir := t.TempDir()
	newAgent := func(m *llmtest.Scripted) *agent.Agent {
		c, err := file.New(dir, file.Config{})
		if err != nil {
			t.Fatal(err)
		}
		a, err := orchestrix.NewWithProvider(m, orchestrix.WithCheckpointer(c),
			orchestrix.WithPermissionMode(permission.ModeAcceptEdits))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	drain := func(th *agent.Thread, text string) {
		for _, err := range th.Prompt(t.Context(), text) {
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	first := newAgent(writeThenAnswer("/notes.txt", "kept"))
	th, err := first.Thread(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	drain(th, "write")

	reader := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "r1", Name: "read_file", Input: map[string]string{"path": "/notes.txt"}}}},
		llmtest.Turn{Text: "read"},
	)
	second := newAgent(reader)
	th2, err := second.Thread(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	drain(th2, "read it")

	var got string
	for _, msg := range reader.Requests[1].Messages {
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolResult && b.ToolUseID == "r1" {
				for _, c := range b.Content {
					got += c.Text
				}
			}
		}
	}
	if !strings.Contains(got, "kept") {
		t.Errorf("read_file result = %q, want the file content written by the first agent", got)
	}
}

func TestWithInterruptOnPauses(t *testing.T) {
	a, err := orchestrix.NewWithProvider(writeThenAnswer("/a.txt", "x"),
		orchestrix.WithInterruptOn("write_file"),
		orchestrix.WithPermissionMode(permission.ModeBypass),
	)
	if err != nil {
		t.Fatal(err)
	}
	if final := finalOf(t, a, orchestrix.Text("write")); final.Reason != agent.ReasonInterrupted {
		t.Errorf("reason = %s, want interrupted", final.Reason)
	}
}

func TestWithShellRegistersExecute(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithShell(t.TempDir(), shell.Config{}))
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, a)
	finalOf(t, a, orchestrix.Text("hi"))
	if !slices.Contains(toolNames(p), "execute") {
		t.Errorf("tools %v lack execute", toolNames(p))
	}
}

// recordingExecutor is an in-memory workspace that can run commands: it
// records each one and runs nothing.
type recordingExecutor struct {
	*membackend.Backend
	cmds []string
}

func (r *recordingExecutor) Execute(_ context.Context, cmd string, _ backend.ExecOptions) (backend.ExecResult, error) {
	r.cmds = append(r.cmds, cmd)
	return backend.ExecResult{Output: "ran"}, nil
}

// A backend given with WithBackend that can run commands gets the execute
// tool, as WithShell's does; one that cannot does not.
func TestWithBackendExecutorRegistersExecute(t *testing.T) {
	exec := &recordingExecutor{Backend: testutil.Memory(t)}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "1", Name: "execute", Input: map[string]any{"command": "echo hi"}}}},
		llmtest.Turn{Text: "done"},
	)
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithBackend(exec), orchestrix.WithPermissionMode(permission.ModeBypass))
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("run it"))
	if !slices.Contains(toolNames(p), "execute") {
		t.Errorf("tools %v lack execute", toolNames(p))
	}
	if !slices.Equal(exec.cmds, []string{"echo hi"}) {
		t.Errorf("executed %q, want [echo hi]", exec.cmds)
	}

	plain := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err = orchestrix.NewWithProvider(plain, orchestrix.WithBackend(testutil.Memory(t)))
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))
	if slices.Contains(toolNames(plain), "execute") {
		t.Errorf("tools %v offer execute on a backend that cannot run commands", toolNames(plain))
	}
}

func TestWithJobsOffersBackgroundTools(t *testing.T) {
	tm := mustJobs(t)
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithShell(t.TempDir(), shell.Config{}), orchestrix.WithJobs(tm))
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, a)
	finalOf(t, a, orchestrix.Text("hi"))
	if !slices.Contains(toolNames(p), "check_job") {
		t.Errorf("tools %v lack check_job", toolNames(p))
	}
}

func TestWithJobsRequiresShell(t *testing.T) {
	tm := mustJobs(t)
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithJobs(tm))
	if err == nil || !strings.Contains(err.Error(), "WithJobs") {
		t.Errorf("err = %v, want an error naming WithJobs", err)
	}
}

func TestWithShellAndBackendExclusive(t *testing.T) {
	for name, opt := range map[string]orchestrix.Option{
		"backend": orchestrix.WithBackend(shellBackend(t)),
		"factory": orchestrix.WithBackendFactory(func(string) backend.Backend { return shellBackend(t) }),
	} {
		_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithShell(t.TempDir(), shell.Config{}), opt)
		if err == nil || !strings.Contains(err.Error(), "WithShell") {
			t.Errorf("%s: err = %v, want an error naming WithShell", name, err)
		}
	}
}

func shellBackend(t *testing.T) backend.Backend {
	t.Helper()
	b, err := shell.New(t.TempDir(), shell.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestInvalidSubagentSpecFailsNew(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithSubagents(subagents.Subagent{Name: "bad name"}))
	if err == nil || !strings.Contains(err.Error(), "bad name") {
		t.Errorf("err = %v, want the invalid spec reported", err)
	}
}

func TestSubagentSpecsAppendToConfig(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithSubagentConfig(subagents.Config{DisableFork: true}),
		orchestrix.WithSubagents(subagents.Subagent{Name: "reviewer", Description: "reviews code", SystemPrompt: "Review."}),
	)
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))
	var desc string
	for _, ts := range p.Requests[0].Tools {
		if ts.Name == subagents.ToolTask {
			desc = ts.Description
		}
	}
	if !strings.Contains(desc, "reviewer") || strings.Contains(desc, "- "+subagents.Fork+":") {
		t.Errorf("task description %q should list reviewer and not fork", desc)
	}
}

func TestWithSummarizeReplacesDefault(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithSummarize(summarize.Config{DisableOffload: true}))
	if err != nil {
		t.Fatal(err)
	}
	if got := stackNames(a)[0]; got != "summarize" {
		t.Errorf("first middleware = %q, want summarize", got)
	}
}

func TestWithSummarizeRejectsBadConfig(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithSummarize(summarize.Config{KeepFraction: 1}))
	if err == nil || !strings.Contains(err.Error(), "WithSummarize: summarize: KeepFraction") {
		t.Fatalf("err = %v, want the WithSummarize validation error", err)
	}
}

func TestWithJobsOnFactoryBackendOffersBackgroundExecute(t *testing.T) {
	testutil.RequireUnixShell(t)
	tm := mustJobs(t)
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e1", Name: "execute",
			Input: map[string]any{"command": "echo hi", "run_in_background": true}}}},
		llmtest.Turn{Text: "ok"},
	)
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithBackendFactory(func(string) backend.Backend { return shellBackend(t) }),
		orchestrix.WithJobs(tm),
		orchestrix.WithPermissionMode(permission.ModeBypass),
	)
	if err != nil {
		t.Fatal(err)
	}
	in := orchestrix.Text("run it")
	in.ThreadID = "t1"
	finalOf(t, a, in)

	var schema string
	for _, ts := range p.Requests[0].Tools {
		if ts.Name == "execute" {
			schema = fmt.Sprint(ts.InputSchema)
		}
	}
	if !strings.Contains(schema, "run_in_background") {
		t.Fatalf("execute schema %q lacks run_in_background", schema)
	}
	for _, b := range p.Requests[1].Messages[len(p.Requests[1].Messages)-1].Content {
		if b.Type == llm.BlockToolResult && b.IsError {
			t.Errorf("background execute failed: %+v", b.Content)
		}
	}
}

func TestWithShellRejectsNegativeTimeout(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithShell(t.TempDir(), shell.Config{Timeout: -1}))
	if err == nil || !strings.Contains(err.Error(), "WithShell: shell: Timeout is negative") {
		t.Errorf("err = %v, want one containing %q", err, "WithShell: shell: Timeout is negative")
	}
}

// mustJobs returns a jobs middleware that is closed when the test ends.
func mustJobs(t *testing.T) *jobs.Middleware {
	t.Helper()
	m, err := jobs.New(jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}
