package orchestrix_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	backendmemory "github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/compress"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/middleware/profiles"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
	"github.com/aswin-kevin/orchestrix/middleware/web"
	"github.com/aswin-kevin/orchestrix/permission"
)

func TestWithAsyncFollowsSubagents(t *testing.T) {
	m, err := async.New(async.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close(context.Background()) }()
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithAsync(m))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"summarize", "cache", "filesystem", "planning", "subagents", "async"}
	if got := stackNames(a); !slices.Equal(got, want) {
		t.Errorf("stack = %v, want %v", got, want)
	}
	if _, ok := a.Tool(async.ToolStart); !ok {
		t.Errorf("no %s tool", async.ToolStart)
	}
}

func TestWithAsyncRejectsNil(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithAsync(nil))
	if err == nil || !strings.Contains(err.Error(), "WithAsync") {
		t.Errorf("err = %v, want an error naming WithAsync", err)
	}
}

// passRubric is a rubric configuration whose grader always passes.
func passRubric() rubric.Config {
	return rubric.Config{
		Criteria: []string{"Answers the question."},
		Grader: rubric.GraderFunc(func(context.Context, rubric.GradeRequest) (rubric.Grade, error) {
			return rubric.Grade{Pass: true}, nil
		}),
	}
}

func TestStackOrderM4(t *testing.T) {
	tm := mustJobs(t)
	am, err := async.New(async.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = am.Close(context.Background()) }()
	a, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.WithShell(t.TempDir(), shell.Config{}),
		orchestrix.WithJobs(tm),
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
	)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, a)
	want := []string{"summarize", "compress", "cache", "hooks", "hitl", "rubric", "memory", "skills", "planmode",
		"filesystem", "planning", "subagents", "async", "deferred", "mcp", "web", "askuser", "jobs", "profiles"}
	if got := stackNames(a); !slices.Equal(got, want) {
		t.Errorf("stack = %v, want %v", got, want)
	}

	def, err := orchestrix.NewWithProvider(llmtest.New())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stackNames(def), []string{"summarize", "cache", "filesystem", "planning", "subagents"}; !slices.Equal(got, want) {
		t.Errorf("default stack = %v, want %v", got, want)
	}
}

// kvBackend returns a kv backend over a fresh in-memory store.
func kvBackend(t *testing.T) *kv.Backend {
	t.Helper()
	b, err := kv.New(testutil.MemoryStore(t), "test")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dirMemories returns a kv backend over a DirStore in dir, namespace "memories".
func dirMemories(t *testing.T, dir string) *kv.Backend {
	t.Helper()
	store, err := kv.NewDirStore(dir, kv.DirStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := kv.New(store, "memories")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runThread runs text on thread id and returns the Final, failing unless it
// completed.
func runThread(t *testing.T, a *agent.Agent, id, text string) *agent.Final {
	t.Helper()
	in := orchestrix.Text(text)
	in.ThreadID = id
	final := finalOf(t, a, in)
	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("thread %s: final = %s (%v), want completed", id, final.Reason, final.Err)
	}
	return final
}

// mustFileStore returns a FileStore under memory.DefaultDir.
func mustFileStore(t *testing.T) *memory.FileStore {
	t.Helper()
	fs, err := memory.NewFileStore(memory.FileStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// recallColor scripts one recall_memory call for "color" and an answer.
func recallColor() []llmtest.Turn {
	return []llmtest.Turn{
		{ToolCalls: []llmtest.Call{{ID: "recall", Name: memory.ToolRecall, Input: memory.RecallInput{Query: "color"}}}},
		{Text: "recalled"},
	}
}

func TestWithMountPersistsMemoriesAcrossThreads(t *testing.T) {
	dir := t.TempDir()
	opts := func(kvb backend.Backend) []orchestrix.Option {
		return []orchestrix.Option{
			orchestrix.WithPermissionMode(permission.ModeBypass),
			orchestrix.WithMemory(memory.Config{Store: mustFileStore(t)}),
			orchestrix.WithMount(memory.DefaultDir, kvb),
		}
	}
	turns := []llmtest.Turn{
		{ToolCalls: []llmtest.Call{
			{ID: "rec", Name: memory.ToolRecord, Input: memory.RecordInput{
				Name: "favorite color", Description: "The user's favorite color.", Type: memory.TypeUser,
				Content: "The user's favorite color is teal.",
			}},
			{ID: "scratch", Name: "write_file", Input: map[string]string{"path": "/scratch.txt", "content": "temp"}},
		}},
		{Text: "noted"},
	}
	a, err := orchestrix.NewWithProvider(llmtest.New(append(turns, recallColor()...)...), opts(dirMemories(t, dir))...)
	if err != nil {
		t.Fatal(err)
	}
	runThread(t, a, "a", "Remember that my favorite color is teal.")
	final := runThread(t, a, "b", "What is my favorite color?")
	if got := resultOf(t, final.State, "recall"); !strings.Contains(got, "teal") {
		t.Errorf("thread b recalled %q, want the memory recorded in thread a", got)
	}

	ctx := context.Background()
	factory := a.Config().BackendFactory
	if _, err := factory("a").Read(ctx, "/scratch.txt", 0, 0); err != nil {
		t.Errorf("thread a lost its own /scratch.txt: %v", err)
	}
	if _, err := factory("b").Read(ctx, "/scratch.txt", 0, 0); err == nil {
		t.Error("thread b sees /scratch.txt written in thread a; the rest of the workspace must stay per thread")
	}

	// A new agent over a fresh DirStore on the same directory, as after a restart.
	a2, err := orchestrix.NewWithProvider(llmtest.New(recallColor()...), opts(dirMemories(t, dir))...)
	if err != nil {
		t.Fatal(err)
	}
	final = runThread(t, a2, "c", "What is my favorite color?")
	if got := resultOf(t, final.State, "recall"); !strings.Contains(got, "teal") {
		t.Errorf("a restarted agent recalled %q, want the stored memory", got)
	}
}

func TestWithMountKeepsShellExecute(t *testing.T) {
	testutil.RequireUnixShell(t)
	root := t.TempDir()
	kvb := kvBackend(t)
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "x", Name: "execute", Input: map[string]string{"command": "echo hi > out.txt"}},
			{ID: "w", Name: "write_file", Input: map[string]string{"path": "/kv/note.txt", "content": "mounted"}},
		}},
		llmtest.Turn{Text: "done"},
	)
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithPermissionMode(permission.ModeBypass),
		orchestrix.WithShell(root, shell.Config{}),
		orchestrix.WithMount("/kv", kvb),
	)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, a)
	finalOf(t, a, orchestrix.Text("go"))
	if !slices.Contains(toolNames(p), "execute") {
		t.Fatalf("tools %v lack execute", toolNames(p))
	}
	if got, err := os.ReadFile(filepath.Join(root, "out.txt")); err != nil || string(got) != "hi\n" {
		t.Errorf("out.txt = %q, %v; want the command to have run in the shell root", got, err)
	}
	if _, err := kvb.Read(context.Background(), "/note.txt", 0, 0); err != nil {
		t.Errorf("/kv/note.txt did not reach the mounted backend: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "kv")); !os.IsNotExist(err) {
		t.Errorf("the mounted file was written to the shell root (stat err %v)", err)
	}
}

func TestWithMountWrapsBackendAndFactory(t *testing.T) {
	ctx := context.Background()
	for name, opt := range map[string]orchestrix.Option{
		"backend": orchestrix.WithBackend(testutil.Memory(t)),
		"factory": orchestrix.WithBackendFactory(func(string) backend.Backend { return testutil.Memory(t) }),
	} {
		kvb := kvBackend(t)
		a, err := orchestrix.NewWithProvider(llmtest.New(), opt, orchestrix.WithMount("/kv", kvb))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b := a.Config().Backend
		if f := a.Config().BackendFactory; f != nil {
			b = f("t1")
		}
		if err := b.Write(ctx, "/kv/a.txt", []byte("x")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := kvb.Read(ctx, "/a.txt", 0, 0); err != nil {
			t.Errorf("%s: /kv/a.txt did not reach the mounted backend: %v", name, err)
		}
	}
}

// closeCounter is an in-memory backend that counts its Close calls.
type closeCounter struct {
	*backendmemory.Backend
	closed int
}

func (c *closeCounter) Close() error {
	c.closed++
	return nil
}

// Agent.Close closes a WithBackend backend that WithMount wrapped and the
// mounted backends, each exactly once (through the composite's Close).
func TestWithMountClosesEachBackendOnce(t *testing.T) {
	inner := &closeCounter{Backend: testutil.Memory(t)}
	mounted := &closeCounter{Backend: testutil.Memory(t)}
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithBackend(inner),
		orchestrix.WithMount("/kv", mounted))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inner.closed != 1 || mounted.closed != 1 {
		t.Errorf("closed backend %d, mount %d times; want 1 each", inner.closed, mounted.closed)
	}
}

func TestWithMountInvalidPrefixFailsNew(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithMount("/", kvBackend(t)))
	if err == nil || !strings.Contains(err.Error(), "WithMount") {
		t.Errorf("err = %v, want an error naming WithMount", err)
	}
}

func TestWithRubricNoCriteriaFailsNew(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithRubric(rubric.Config{}))
	if err == nil || !strings.Contains(err.Error(), "WithRubric") {
		t.Errorf("err = %v, want an error naming WithRubric", err)
	}
}

func TestWithProfilesBadPatternFailsNew(t *testing.T) {
	_, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithProfiles(profiles.Config{
		Adjustments: []profiles.Adjustment{{Models: []string{""}}},
	}))
	if err == nil || !strings.Contains(err.Error(), "WithProfiles") {
		t.Errorf("err = %v, want an error naming WithProfiles", err)
	}
}

func TestWithProfilesExcludesMiddleware(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithConfig(func(c *agent.Config) { c.Model = "small-1" }),
		orchestrix.WithProfiles(profiles.Config{Adjustments: []profiles.Adjustment{
			{Models: []string{"small-*"}, ExcludeMiddleware: []string{"planning"}},
		}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	finalOf(t, a, orchestrix.Text("hi"))
	names := toolNames(p)
	if slices.Contains(names, "write_todos") {
		t.Errorf("tools %v offer write_todos to small-1, whose profile excludes planning", names)
	}
	if !slices.Contains(names, "read_file") {
		t.Errorf("tools %v lack read_file; only planning was excluded", names)
	}
}

// runningHandle is a background task that runs until it is stopped.
type runningHandle struct {
	done chan struct{}
	once sync.Once
}

func (h *runningHandle) Done() <-chan struct{} { return h.done }

func (h *runningHandle) Outcome() (async.Outcome, bool) {
	select {
	case <-h.done:
		return async.Outcome{Status: async.StatusCancelled}, true
	default:
		return async.Outcome{}, false
	}
}

func (h *runningHandle) Progress() string                   { return "" }
func (h *runningHandle) Send(context.Context, string) error { return nil }
func (h *runningHandle) Stop(context.Context) error         { h.once.Do(func() { close(h.done) }); return nil }

// runForever is an async.Runner whose tasks run until stopped.
type runForever struct{}

func (runForever) Start(context.Context, async.Request) (async.Handle, error) {
	return &runningHandle{done: make(chan struct{})}, nil
}

func TestRubricAndAsyncReminderShareContinuations(t *testing.T) {
	am, err := async.New(async.Config{Runner: runForever{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = am.Close(context.Background()) })
	var answers []string
	grader := rubric.GraderFunc(func(_ context.Context, req rubric.GradeRequest) (rubric.Grade, error) {
		answers = append(answers, req.Answer)
		return rubric.Grade{Pass: req.Iteration > 0, Feedback: "Name the source."}, nil
	})
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s", Name: async.ToolStart,
			Input: map[string]string{"description": "Research the topic.", "subagent_type": "general-purpose"}}}},
		llmtest.Turn{Text: "answer 1"},
		llmtest.Turn{Text: "answer 2"},
		llmtest.Turn{Text: "answer 3"},
		llmtest.Turn{Text: "spare: must stay unused"},
	)
	a, err := orchestrix.NewWithProvider(p,
		orchestrix.WithAsync(am),
		orchestrix.WithRubric(rubric.Config{Criteria: []string{"Cites a source."}, Grader: grader}),
	)
	if err != nil {
		t.Fatal(err)
	}
	final := runThread(t, a, "t1", "Research the topic.")
	if final.Text != "answer 3" {
		t.Errorf("final text = %q, want answer 3", final.Text)
	}
	if want := []string{"answer 2", "answer 3"}; !slices.Equal(answers, want) {
		t.Errorf("graded answers = %q, want %q: the async reminder must come before grading", answers, want)
	}
	if p.Calls() != 4 {
		t.Fatalf("model calls = %d, want 4 (start, reminder, feedback, pass)", p.Calls())
	}
	last := func(i int) string {
		msgs := p.Requests[i].Messages
		return msgs[len(msgs)-1].Text()
	}
	if got := last(2); !strings.Contains(got, "async_1") {
		t.Errorf("third request ends with %q, want the async reminder", got)
	}
	if got := last(3); !strings.Contains(got, "rubric_feedback") {
		t.Errorf("fourth request ends with %q, want the rubric feedback", got)
	}
}
