package orchestrix_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
)

// finalOf drains a run and returns its Final.
func finalOf(t *testing.T, a *agent.Agent, in agent.Input) *agent.Final {
	t.Helper()
	var last agent.Event
	for ev := range a.Run(context.Background(), in) {
		last = ev
	}
	if last.Kind != agent.KindFinal || last.Final == nil {
		t.Fatalf("last event = %+v, want KindFinal", last)
	}
	return last.Final
}

func writeThenAnswer(path, content string) *llmtest.Scripted {
	return llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{
			ID: "c1", Name: "write_file", Input: map[string]string{"path": path, "content": content},
		}}},
		llmtest.Turn{Text: "done"},
	)
}

func TestNewWithScriptedProviderWritesFile(t *testing.T) {
	mem := memory.New()
	a, err := orchestrix.New(writeThenAnswer("/hello.txt", "hi"),
		orchestrix.WithBackend(mem),
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
	)
	if err != nil {
		t.Fatal(err)
	}

	final := finalOf(t, a, orchestrix.Text("write a file"))
	if final.Reason != agent.ReasonCompleted || final.Text != "done" {
		t.Fatalf("final = %s %q (err %v), want completed %q", final.Reason, final.Text, final.Err, "done")
	}
	got, err := mem.Read(context.Background(), "/hello.txt", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 1 || got.Lines[0] != "hi" {
		t.Fatalf("file lines = %q, want [hi]", got.Lines)
	}
}

func TestDefaultBackendKeepsFilesPerThread(t *testing.T) {
	a, err := orchestrix.New(llmtest.New(), orchestrix.WithPermissionMode(permission.ModeAcceptEdits))
	if err != nil {
		t.Fatal(err)
	}
	factory := a.Config().BackendFactory
	if factory == nil {
		t.Fatal("default BackendFactory is nil")
	}
	if factory("t1") != factory("t1") {
		t.Error("same thread id got different backends; files would be lost between runs")
	}
	if factory("t1") == factory("t2") {
		t.Error("different thread ids share a backend")
	}
}

// writeThenReadInSeparateRuns runs two runs of one agent on thread tid, the
// first writing /secret.txt and the second reading it, and returns the tool
// result of the read.
func writeThenReadInSeparateRuns(t *testing.T, tid string) llm.Block {
	t.Helper()
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w", Name: "write_file", Input: map[string]string{"path": "/secret.txt", "content": "s3cret"}}}},
		llmtest.Turn{Text: "written"},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "r", Name: "read_file", Input: map[string]string{"path": "/secret.txt"}}}},
		llmtest.Turn{Text: "read"},
	)
	a, err := orchestrix.New(p, orchestrix.WithPermissionMode(permission.ModeAcceptEdits))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		in := orchestrix.Text("go")
		in.ThreadID = tid
		final := finalOf(t, a, in)
		if final.Reason != agent.ReasonCompleted {
			t.Fatalf("run %d: %s (err %v)", i, final.Reason, final.Err)
		}
		if i == 1 {
			for _, m := range final.State.Messages() {
				for _, b := range m.Content {
					if b.Type == llm.BlockToolResult && b.ToolUseID == "r" {
						return b
					}
				}
			}
			t.Fatal("read_file produced no tool result")
		}
	}
	return llm.Block{}
}

func blockText(b llm.Block) string {
	var s string
	for _, c := range b.Content {
		s += c.Text
	}
	return s
}

func TestThreadlessRunsIsolated(t *testing.T) {
	res := writeThenReadInSeparateRuns(t, "")
	if !res.IsError || strings.Contains(blockText(res), "s3cret") {
		t.Fatalf("a threadless run saw another run's file: error=%v %q", res.IsError, blockText(res))
	}
}

func TestThreadedRunKeepsFiles(t *testing.T) {
	res := writeThenReadInSeparateRuns(t, "t1")
	if res.IsError || !strings.Contains(blockText(res), "s3cret") {
		t.Fatalf("a threaded run lost its file: error=%v %q", res.IsError, blockText(res))
	}
}

func TestThreadlessFactoryNeverShares(t *testing.T) {
	a, err := orchestrix.New(llmtest.New(), orchestrix.WithPermissionMode(permission.ModeAcceptEdits))
	if err != nil {
		t.Fatal(err)
	}
	f := a.Config().BackendFactory
	if f("") == f("") {
		t.Error("the empty thread id got a shared backend")
	}
}

func toolNames(p *llmtest.Scripted) []string {
	var names []string
	for _, ts := range p.Requests[0].Tools {
		names = append(names, ts.Name)
	}
	return names
}

func TestWithoutMiddlewarePlanning(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.New(p, orchestrix.WithoutMiddleware("planning"))
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))

	names := strings.Join(toolNames(p), ",")
	if strings.Contains(names, "write_todos") {
		t.Errorf("tools %q include write_todos after removing planning", names)
	}
	if !strings.Contains(names, "write_file") {
		t.Errorf("tools %q lost the filesystem tools", names)
	}
}

func TestPlanningIncludedByDefault(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.New(p)
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))
	if !strings.Contains(strings.Join(toolNames(p), ","), "write_todos") {
		t.Errorf("default tools %v lack write_todos", toolNames(p))
	}
}

func TestBadRuleErrors(t *testing.T) {
	_, err := orchestrix.New(llmtest.New(),
		orchestrix.WithAllow("read_file("),
		orchestrix.WithDeny(""),
	)
	if err == nil {
		t.Fatal("New accepted malformed rules")
	}
	if !strings.Contains(err.Error(), "read_file(") {
		t.Errorf("error %q does not mention the first bad rule", err)
	}
}

func TestRulesReachConfig(t *testing.T) {
	a, err := orchestrix.New(llmtest.New(),
		orchestrix.WithAllow("read_file"), orchestrix.WithAsk("write_file"), orchestrix.WithDeny("execute"))
	if err != nil {
		t.Fatal(err)
	}
	r := a.Config().Rules
	if len(r.Allow) != 1 || len(r.Ask) != 1 || len(r.Deny) != 1 {
		t.Errorf("rules = %+v, want one of each", r)
	}
}

func TestUnknownPermissionModeErrors(t *testing.T) {
	_, err := orchestrix.New(llmtest.New(), orchestrix.WithPermissionMode("acceptEdit"))
	if err == nil {
		t.Fatal("New accepted a mistyped permission mode")
	}
	for _, want := range []string{"acceptEdit", "acceptEdits", "plan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestBackendAndFactoryConflict(t *testing.T) {
	_, err := orchestrix.New(llmtest.New(),
		orchestrix.WithBackend(memory.New()),
		orchestrix.WithBackendFactory(func(string) backend.Backend { return memory.New() }),
	)
	if err == nil {
		t.Fatal("New accepted both WithBackend and WithBackendFactory")
	}
}

func TestInvalidModel(t *testing.T) {
	for name, model := range map[string]any{"nil": nil, "int": 42} {
		_, err := orchestrix.New(model)
		if err == nil || !strings.Contains(err.Error(), "model must be a model spec string or an llm.Provider") {
			t.Errorf("%s: err = %v, want model type error", name, err)
		}
	}
}

func TestModelTimeoutReachesConfig(t *testing.T) {
	a, err := orchestrix.New(llmtest.New(), orchestrix.WithModelTimeout(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Config().ModelTimeout; got != time.Minute {
		t.Errorf("Config.ModelTimeout = %v, want 1m", got)
	}
}

func TestStringModelSpec(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		http.Error(w, "stop here", http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "test-key")

	a, err := orchestrix.New("openai:x")
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Config().Model; got != "x" {
		t.Errorf("Config.Model = %q, want x", got)
	}
	final := finalOf(t, a, orchestrix.Text("hi"))
	if final.Err == nil {
		t.Error("run against the failing server reported no error")
	}
	if !strings.Contains(gotBody, `"x"`) {
		t.Errorf("request body %q does not name model x", gotBody)
	}
}

func TestBadModelSpec(t *testing.T) {
	if _, err := orchestrix.New("nocolon"); err == nil {
		t.Error("New accepted a spec without a format prefix")
	}
}

func TestCacheBoundaryCoversSystemSegments(t *testing.T) {
	a, err := orchestrix.New(llmtest.New(), orchestrix.WithSystemPrompt("one", "two"))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Config().CacheBoundary; got != 2 {
		t.Errorf("CacheBoundary = %d, want 2", got)
	}
}

func TestLimitOptionsAndWithConfig(t *testing.T) {
	a, err := orchestrix.New(llmtest.New(),
		orchestrix.WithMaxTurns(7), orchestrix.WithMaxTokens(99), orchestrix.WithWorkingDir("/w"),
		orchestrix.WithConfig(func(c *agent.Config) { c.MaxTurns = 9 }),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := a.Config()
	if c.MaxTurns != 9 || c.MaxTokens != 99 || c.WorkingDir != "/w" {
		t.Errorf("config = turns %d tokens %d dir %q, want 9 99 /w (WithConfig last)", c.MaxTurns, c.MaxTokens, c.WorkingDir)
	}
}

func TestText(t *testing.T) {
	in := orchestrix.Text("hello")
	if len(in.Messages) != 1 || in.Messages[0].Text() != "hello" {
		t.Errorf("Text = %+v", in)
	}
}
