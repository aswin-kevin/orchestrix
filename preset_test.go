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
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
	"github.com/aswin-kevin/orchestrix/permission"
)

func stackNames(a *agent.Agent) []string {
	var names []string
	for _, m := range a.Config().Middleware {
		names = append(names, m.Name())
	}
	return names
}

func TestDefaultStackOrder(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"summarize", "cache", "filesystem", "planning", "subagents"}
	if got := stackNames(a); !slices.Equal(got, want) {
		t.Errorf("default stack = %v, want %v", got, want)
	}
}

func TestFullStackOrder(t *testing.T) {
	sh := t.TempDir()
	tm := tasks.New(tasks.Config{})
	defer tm.Shutdown()
	a, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.WithShell(sh, shell.Options{}),
		orchestrix.WithTasks(tm),
		orchestrix.WithHooks(hooks.Config{}),
		orchestrix.WithInterruptOn("write_file"),
		orchestrix.WithCheckpointer(mustFileCheckpointer(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, a)
	want := []string{"summarize", "cache", "hooks", "hitl", "filesystem", "planning", "subagents", "tasks"}
	if got := stackNames(a); !slices.Equal(got, want) {
		t.Errorf("full stack = %v, want %v", got, want)
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
	c, err := file.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaultWorkspaceSurvivesCheckpoint(t *testing.T) {
	dir := t.TempDir()
	newAgent := func(m *llmtest.Scripted) *agent.Agent {
		c, err := file.New(dir)
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
		for _, err := range th.Prompt(context.Background(), text) {
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	first := newAgent(writeThenAnswer("/notes.txt", "kept"))
	th, err := first.Thread(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	drain(th, "write")

	reader := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "r1", Name: "read_file", Input: map[string]string{"path": "/notes.txt"}}}},
		llmtest.Turn{Text: "read"},
	)
	second := newAgent(reader)
	th2, err := second.Thread(context.Background(), "t1")
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
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithShell(t.TempDir(), shell.Options{}))
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
	exec := &recordingExecutor{Backend: membackend.New()}
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
	a, err = orchestrix.NewWithProvider(plain, orchestrix.WithBackend(membackend.New()))
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))
	if slices.Contains(toolNames(plain), "execute") {
		t.Errorf("tools %v offer execute on a backend that cannot run commands", toolNames(plain))
	}
}

func TestWithTasksOffersBackgroundTools(t *testing.T) {
	tm := tasks.New(tasks.Config{})
	defer tm.Shutdown()
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithShell(t.TempDir(), shell.Options{}), orchestrix.WithTasks(tm))
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, a)
	finalOf(t, a, orchestrix.Text("hi"))
	if !slices.Contains(toolNames(p), "task_output") {
		t.Errorf("tools %v lack task_output", toolNames(p))
	}
}

func TestWithTasksRequiresShell(t *testing.T) {
	tm := tasks.New(tasks.Config{})
	defer tm.Shutdown()
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithTasks(tm))
	if err == nil || !strings.Contains(err.Error(), "WithTasks") {
		t.Errorf("err = %v, want an error naming WithTasks", err)
	}
}

func TestWithShellAndBackendExclusive(t *testing.T) {
	for name, opt := range map[string]orchestrix.Option{
		"backend": orchestrix.WithBackend(shellBackend(t)),
		"factory": orchestrix.WithBackendFactory(func(string) backend.Backend { return shellBackend(t) }),
	} {
		_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithShell(t.TempDir(), shell.Options{}), opt)
		if err == nil || !strings.Contains(err.Error(), "WithShell") {
			t.Errorf("%s: err = %v, want an error naming WithShell", name, err)
		}
	}
}

func shellBackend(t *testing.T) backend.Backend {
	t.Helper()
	b, err := shell.New(t.TempDir(), shell.Options{})
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

func TestWithTasksOnFactoryBackendOffersBackgroundExecute(t *testing.T) {
	testutil.RequireUnixShell(t)
	tm := tasks.New(tasks.Config{})
	defer tm.Shutdown()
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e1", Name: "execute",
			Input: map[string]any{"command": "echo hi", "run_in_background": true}}}},
		llmtest.Turn{Text: "ok"},
	)
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithBackendFactory(func(string) backend.Backend { return shellBackend(t) }),
		orchestrix.WithTasks(tm),
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
