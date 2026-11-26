package orchestrix_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
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

// closeAtEnd closes a when the test ends, releasing its backend before
// t.TempDir's cleanup, which cannot remove a directory still open on
// Windows.
func closeAtEnd(t *testing.T, a *agent.Agent) {
	t.Helper()
	t.Cleanup(func() {
		if err := a.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
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
	mem := testutil.Memory(t)
	a, err := orchestrix.NewWithProvider(writeThenAnswer("/hello.txt", "hi"),
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
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithPermissionMode(permission.ModeAcceptEdits))
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
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithPermissionMode(permission.ModeAcceptEdits))
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
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithPermissionMode(permission.ModeAcceptEdits))
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
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithoutMiddleware("planning"))
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
	a, err := orchestrix.NewWithProvider(p)
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))
	if !strings.Contains(strings.Join(toolNames(p), ","), "write_todos") {
		t.Errorf("default tools %v lack write_todos", toolNames(p))
	}
}

func TestBadRuleErrors(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(),
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
	a, err := orchestrix.NewWithProvider(llmtest.New(),
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
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithPermissionMode("acceptEdit"))
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
	_, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.WithBackend(testutil.Memory(t)),
		orchestrix.WithBackendFactory(func(string) backend.Backend { return testutil.Memory(t) }),
	)
	if err == nil {
		t.Fatal("New accepted both WithBackend and WithBackendFactory")
	}
}

func TestNewWithProvider(t *testing.T) {
	p := llmtest.New()
	a, err := orchestrix.NewWithProvider(p, orchestrix.WithMaxTurns(3))
	if err != nil {
		t.Fatal(err)
	}
	cfg := a.Config()
	if cfg.Provider != llm.Provider(p) {
		t.Errorf("Provider = %v, want the one passed in", cfg.Provider)
	}
	if cfg.Model != "" || cfg.MaxTurns != 3 {
		t.Errorf("Model = %q, MaxTurns = %d; want empty model and 3", cfg.Model, cfg.MaxTurns)
	}
}

func TestNewWithProviderNil(t *testing.T) {
	a, err := orchestrix.NewWithProvider(nil)
	if err == nil || !strings.Contains(err.Error(), "provider must not be nil") {
		t.Fatalf("err = %v, want nil-provider error", err)
	}
	if a != nil {
		t.Error("agent returned alongside the error")
	}
	// Option problems are still joined with it.
	_, err = orchestrix.NewWithProvider(nil, orchestrix.WithPermissionMode("bogus"))
	if err == nil || !strings.Contains(err.Error(), "provider must not be nil") || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("err = %v, want both errors joined", err)
	}
}

func TestModelTimeoutReachesConfig(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithModelTimeout(time.Minute))
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
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithSystemPrompt("one", "two"))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Config().CacheBoundary; got != 2 {
		t.Errorf("CacheBoundary = %d, want 2", got)
	}
}

func TestLimitOptionsAndWithConfig(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New(),
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

// stoppableProcess is a backend.Process that runs until it is stopped.
type stoppableProcess struct {
	done chan struct{}
	once sync.Once
}

func (p *stoppableProcess) Done() <-chan struct{} { return p.done }
func (p *stoppableProcess) Result() (backend.ExecResult, bool) {
	select {
	case <-p.done:
		return backend.ExecResult{ExitCode: -1}, true
	default:
		return backend.ExecResult{}, false
	}
}
func (p *stoppableProcess) Output() string { return "" }
func (p *stoppableProcess) Stop(context.Context) error {
	p.once.Do(func() { close(p.done) })
	return nil
}

// startingBackend is a memory backend that can start commands; each one runs
// until it is stopped.
type startingBackend struct {
	*memory.Backend
	mu    sync.Mutex
	procs []*stoppableProcess
}

func (b *startingBackend) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	return backend.ExecResult{}, nil
}

func (b *startingBackend) Start(context.Context, string, backend.ExecOptions) (backend.Process, error) {
	p := &stoppableProcess{done: make(chan struct{})}
	b.mu.Lock()
	b.procs = append(b.procs, p)
	b.mu.Unlock()
	return p, nil
}

func TestOrchestrixCloseStopsMCPAndTasks(t *testing.T) {
	m := connectMCP(t, false)
	tm := mustJobs(t)
	b := &startingBackend{Backend: testutil.Memory(t)}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e1", Name: filesystem.ToolExecute,
			Input: map[string]any{"command": "serve", "run_in_background": true}}}},
		llmtest.Turn{Text: "started"},
	)
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithBackend(b),
		orchestrix.WithJobs(tm),
		orchestrix.WithMCP(m),
		orchestrix.WithPermissionMode(permission.ModeBypass),
	)
	if err != nil {
		t.Fatal(err)
	}
	in := orchestrix.Text("start the server")
	in.ThreadID = "t1"
	if final := finalOf(t, a, in); final.Reason != agent.ReasonCompleted {
		t.Fatalf("run: %s (err %v)", final.Reason, final.Err)
	}
	b.mu.Lock()
	procs := slices.Clone(b.procs)
	b.mu.Unlock()
	if len(procs) != 1 {
		t.Fatalf("started %d jobs, want 1", len(procs))
	}
	if len(m.DynamicTools()) == 0 {
		t.Fatal("MCP server offers no tools before Close")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-procs[0].Done():
	default:
		t.Error("background job still running after Close")
	}
	if st := m.Status(); len(st) != 1 || st[0].Connected {
		t.Errorf("MCP status after Close = %+v, want the server disconnected", st)
	}
	if final := finalOf(t, a, in); !errors.Is(final.Err, agent.ErrClosed) {
		t.Errorf("run after Close: err = %v, want ErrClosed", final.Err)
	}
}

func TestOrchestrixCloseDropsThreadWorkspaces(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New())
	if err != nil {
		t.Fatal(err)
	}
	factory := a.Config().BackendFactory
	before := factory("t1")
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if factory("t1") == before {
		t.Error("the thread's workspace is still kept after Close")
	}
}
