package orchestrix_test

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	membackend "github.com/aswin-kevin/orchestrix/backend/memory"
	checkpointfile "github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/coordinator"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/compress"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/middleware/profiles"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/middleware/web"
	"github.com/aswin-kevin/orchestrix/permission"
)

const (
	// liveModelEnv selects the model for the live tests; they are skipped
	// when it is unset.
	liveModelEnv = "ORX_TEST_MODEL"
	// liveTimeout bounds one live test, which may involve several model turns.
	liveTimeout = 3 * time.Minute
)

// liveModel returns the model spec for a live test, or skips the test. It
// maps ORX_TEST_BASE_URL and ORX_TEST_API_KEY onto the environment variables
// the model's format reads, so they apply to this test only.
func liveModel(t *testing.T) string {
	t.Helper()
	spec := os.Getenv(liveModelEnv)
	if spec == "" {
		t.Skipf("%s not set; skipping live test", liveModelEnv)
	}
	cfg, err := llm.ParseModel(spec)
	if err != nil {
		t.Fatalf("%s: %v", liveModelEnv, err)
	}
	prefix := "OPENAI"
	if cfg.Format == llm.FormatAnthropic {
		prefix = "ANTHROPIC"
	}
	if v := os.Getenv("ORX_TEST_BASE_URL"); v != "" {
		t.Setenv(prefix+"_BASE_URL", v)
	}
	if v := os.Getenv("ORX_TEST_API_KEY"); v != "" {
		t.Setenv(prefix+"_API_KEY", v)
	}
	return spec
}

func TestLiveAgentWritesAndReadsFile(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	ws := membackend.New()
	a, err := orchestrix.New(spec,
		orchestrix.WithBackend(ws),
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	prompt := "Create /notes/hello.txt containing exactly `orchestrix ok`, " +
		"then read it back and reply with its contents."
	var final *agent.Final
	wroteFile := false
	for ev, err := range a.Run(ctx, orchestrix.Text(prompt)) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case agent.KindToolUse:
			wroteFile = wroteFile || ev.ToolName == "write_file"
		case agent.KindFinal:
			final = ev.Final
		}
	}

	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %+v, want reason %q", final, agent.ReasonCompleted)
	}
	if !wroteFile {
		t.Error("no write_file tool use was observed")
	}
	got, err := ws.Read(ctx, "/notes/hello.txt", 0, 10)
	if err != nil {
		t.Fatalf("reading the file the agent should have written: %v", err)
	}
	if content := strings.TrimSpace(strings.Join(got.Lines, "\n")); content != "orchestrix ok" {
		t.Errorf("file content = %q, want %q", content, "orchestrix ok")
	}
}

func TestLiveStreamingText(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	a, err := orchestrix.New(spec, orchestrix.WithMaxTurns(2))
	if err != nil {
		t.Fatal(err)
	}

	textEvents := 0
	var final *agent.Final
	for ev, err := range a.Run(ctx, orchestrix.Text("Count from one to ten in words, separated by commas.")) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case agent.KindText:
			textEvents++
		case agent.KindFinal:
			final = ev.Final
		}
	}

	if textEvents < 2 {
		t.Errorf("got %d text events, want a streamed reply of at least 2", textEvents)
	}
	if final == nil || final.Usage.Output <= 0 {
		t.Errorf("final usage = %+v, want Output > 0", final)
	}
}

// drain consumes a run, failing the test on a stream error, and returns its
// Final event and every event seen.
func drain(t *testing.T, run iter.Seq2[agent.Event, error]) (*agent.Final, []agent.Event) {
	t.Helper()
	var final *agent.Final
	var events []agent.Event
	for ev, err := range run {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	return final, events
}

// requireCompleted fails the test unless the run ended with ReasonCompleted.
func requireCompleted(t *testing.T, final *agent.Final) {
	t.Helper()
	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %+v, want reason %q", final, agent.ReasonCompleted)
	}
}

// readLimit is how many lines readTrimmed reads; the files the live tests
// check are one line long.
const readLimit = 10

// readTrimmed returns the content of path in ws without surrounding space.
func readTrimmed(ctx context.Context, t *testing.T, ws backend.Backend, path string) string {
	t.Helper()
	got, err := ws.Read(ctx, path, 0, readLimit)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.TrimSpace(strings.Join(got.Lines, "\n"))
}

func TestLiveSubagentDelegation(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	ws := membackend.New()
	a, err := orchestrix.New(spec,
		orchestrix.WithBackend(ws),
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	prompt := "Use the task tool with subagent_type general-purpose to create " +
		"/report.txt containing exactly `delegated ok`. Then reply DONE."
	final, events := drain(t, a.Run(ctx, orchestrix.Text(prompt)))

	requireCompleted(t, final)
	usedTask, sawSubagent := false, false
	for _, ev := range events {
		usedTask = usedTask || (ev.Kind == agent.KindToolUse && ev.ToolName == "task")
		sawSubagent = sawSubagent || ev.Agent == "general-purpose"
	}
	if !usedTask {
		t.Error("no task tool use was observed")
	}
	if !sawSubagent {
		t.Error("no event came from the general-purpose subagent")
	}
	if got := readTrimmed(ctx, t, ws, "/report.txt"); !strings.Contains(strings.ToLower(got), "delegated ok") {
		t.Errorf("file content = %q, want it to contain %q", got, "delegated ok")
	}
}

// Sizes for the summarization test: headroom is how many tokens the
// conversation may use beyond the fixed system prompt and tool definitions,
// so a compaction can always make progress.
const (
	summarizeHeadroomTokens = 4000
	fillerChars             = 6000
)

// fillerText returns about fillerChars characters of unremarkable prose.
func fillerText() string {
	const sentence = "The quick brown fox jumps over the lazy dog while the river runs quietly past the old mill. "
	return strings.Repeat(sentence, fillerChars/len(sentence)+1)[:fillerChars]
}

// minimalSummarizingAgent builds an agent with only the summarize
// middleware and the given context window.
func minimalSummarizingAgent(t *testing.T, spec string, window int) *agent.Agent {
	t.Helper()
	mc, err := llm.ParseModel(spec)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := llm.New(mc)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{
		Provider:   provider,
		Model:      mc.Model,
		Profile:    &llm.Profile{ContextWindow: window, MaxOutput: 1024},
		System:     []string{"You are a terse assistant. Answer in one short line."},
		Middleware: []agent.Middleware{summarize.New(summarize.Config{})},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLiveSummarizationTinyWindow(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	// Measure the fixed cost (system prompt and tools) with a first request
	// so the window leaves a known amount of room for the conversation.
	probe, _ := drain(t, minimalSummarizingAgent(t, spec, llm.DefaultProfile().ContextWindow).
		Run(ctx, orchestrix.Text("Reply with OK.")))
	requireCompleted(t, probe)
	baseline := probe.Usage.Input + probe.Usage.CacheRead + probe.Usage.CacheWrite
	if baseline <= 0 {
		t.Skip("the endpoint reports no input usage; cannot size the context window")
	}

	a := minimalSummarizingAgent(t, spec, baseline+summarizeHeadroomTokens)
	thread, err := a.Thread(ctx, "tiny-window")
	if err != nil {
		t.Fatal(err)
	}
	turns := []string{"The code word is ORCHID. Acknowledge briefly."}
	for range 3 {
		turns = append(turns, "Acknowledge this text in one line:\n"+fillerText())
	}
	turns = append(turns, "What is the code word?")

	var final *agent.Final
	for _, text := range turns {
		final, _ = drain(t, thread.Prompt(ctx, text))
		requireCompleted(t, final)
	}

	b, ok := summarize.Current(thread.State())
	if !ok {
		t.Errorf("no compaction happened (boundary %+v)", b)
	}
	requireTextContains(t, final.Text, "ORCHID")
	if t.Failed() {
		// The summary is the model's own: when it dropped the code word,
		// the model forgot it, not the harness.
		t.Logf("compaction summary: %q", b.Summary)
	}
}

func TestLiveCheckpointResumeAcrossAgents(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	dir := t.TempDir()
	// newAgent stands in for a fresh process: its own checkpointer on the
	// shared directory and its own preset with a per-thread workspace.
	newAgent := func() *agent.Agent {
		cp, err := checkpointfile.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		a, err := orchestrix.New(spec,
			orchestrix.WithCheckpointer(cp),
			orchestrix.WithInterruptOn("write_file"),
			orchestrix.WithMaxTurns(10),
		)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	first, err := newAgent().Thread(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	final, _ := drain(t, first.Prompt(ctx, "Write /a.txt containing `resume ok`."))
	if final == nil || final.Reason != agent.ReasonInterrupted {
		t.Fatalf("final = %+v, want reason %q", final, agent.ReasonInterrupted)
	}

	agentB := newAgent()
	second, err := agentB.Thread(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	intr, ok := second.Pending()
	if !ok {
		t.Fatal("the reloaded thread has no pending interrupt")
	}
	var decisions []agent.Decision
	for _, req := range intr.Requests {
		if req.Tool != "write_file" {
			t.Errorf("pending tool = %q, want write_file", req.Tool)
		}
		decisions = append(decisions, agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Approve})
	}
	final, _ = drain(t, second.Resume(ctx, decisions))
	requireCompleted(t, final)

	final, _ = drain(t, second.Prompt(ctx, "Read /a.txt and reply with its contents."))
	requireCompleted(t, final)
	if got := readTrimmed(ctx, t, agentB.Config().BackendFactory("live"), "/a.txt"); !strings.Contains(strings.ToLower(got), "resume ok") {
		t.Errorf("file content = %q, want it to contain %q", got, "resume ok")
	}
	requireTextContains(t, final.Text, "resume ok")
}

// sawToolUse reports whether events contain a tool use of the named tool.
func sawToolUse(events []agent.Event, name string) bool {
	for _, ev := range events {
		if ev.Kind == agent.KindToolUse && ev.ToolName == name {
			return true
		}
	}
	return false
}

// requireTextContains fails the test unless text contains want, ignoring case
// and invisible format characters: some models put a zero-width space inside
// a word they quote.
func requireTextContains(t *testing.T, text, want string) {
	t.Helper()
	visible := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
	if !strings.Contains(strings.ToLower(visible), strings.ToLower(want)) {
		t.Errorf("final text = %q, want it to contain %q", text, want)
	}
}

func TestLiveSkillLoadAndUse(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "secret-greeting")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := "---\n" +
		"name: secret-greeting\n" +
		"description: Provides the secret greeting. Use when asked for the secret greeting.\n" +
		"---\n" +
		"When asked for the secret greeting, reply with exactly ZEPHYR-42.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillFile), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := orchestrix.New(spec,
		orchestrix.WithBackend(membackend.New()),
		orchestrix.WithSkills(skills.Config{Sources: []skills.Source{skills.DirSource(dir)}}),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	final, events := drain(t, a.Run(ctx, orchestrix.Text("What is the secret greeting?")))
	requireCompleted(t, final)
	if !sawToolUse(events, "skill") {
		t.Error("no skill tool use was observed")
	}
	requireTextContains(t, final.Text, "ZEPHYR-42")
}

func TestLiveMemoryRecall(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	ws := membackend.New()
	entry := "---\n" +
		"name: favorite-color\n" +
		"description: The user's favorite color.\n" +
		"type: user\n" +
		"---\n" +
		"The user's favorite color is teal.\n"
	if err := ws.Write(ctx, memory.DefaultDir+"/favorite-color.md", []byte(entry)); err != nil {
		t.Fatal(err)
	}

	a, err := orchestrix.New(spec,
		orchestrix.WithBackend(ws),
		orchestrix.WithMemory(memory.Config{Store: &memory.StoreConfig{}}),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	final, events := drain(t, a.Run(ctx, orchestrix.Text("Use recall_memory to find my favorite color, then tell me.")))
	requireCompleted(t, final)
	if !sawToolUse(events, "recall_memory") {
		t.Error("no recall_memory tool use was observed")
	}
	requireTextContains(t, final.Text, "teal")
}

func TestLiveMCPInProcess(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	srv := mcp.NewServer(mcp.Implementation{Name: "test", Version: "1"}, "")
	srv.AddTool(mcp.ToolInfo{Name: "lookup_code", Description: "Returns the secret code."},
		func(context.Context, json.RawMessage) (*mcp.CallToolResult, error) {
			return mcp.TextResult("MCP-OK-7781"), nil
		})
	m, err := mcp.Connect(ctx, []mcp.ServerConfig{{Name: "test", Transport: mcp.InProcess(srv)}}, mcp.ConnectConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	a, err := orchestrix.New(spec,
		orchestrix.WithBackend(membackend.New()),
		orchestrix.WithMCP(m),
		orchestrix.WithPermissionMode(permission.ModeBypass),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	final, events := drain(t, a.Run(ctx, orchestrix.Text("Call the lookup_code tool and reply with the code it returns.")))
	requireCompleted(t, final)
	if !sawToolUse(events, "mcp__test__lookup_code") {
		t.Error("no mcp__test__lookup_code tool use was observed")
	}
	requireTextContains(t, final.Text, "MCP-OK-7781")
}

func TestLiveWebFetch(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><head><title>Code</title></head><body><p>The code word is PELICAN.</p></body></html>")
	}))
	defer srv.Close()

	a, err := orchestrix.New(spec,
		orchestrix.WithBackend(membackend.New()),
		orchestrix.WithWeb(web.Config{AllowPrivateNetworks: true, ReadOnly: true}),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	final, events := drain(t, a.Run(ctx, orchestrix.Text("Fetch "+srv.URL+" and tell me the code word.")))
	requireCompleted(t, final)
	if !sawToolUse(events, "web_fetch") {
		t.Error("no web_fetch tool use was observed")
	}
	requireTextContains(t, final.Text, "PELICAN")
}

func TestLivePlanModeApproval(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	cp, err := checkpointfile.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := membackend.New()
	a, err := orchestrix.New(spec,
		orchestrix.WithBackend(ws),
		orchestrix.WithCheckpointer(cp),
		orchestrix.WithPermissionMode(permission.ModePlan),
		orchestrix.WithPlanMode(planmode.Config{DisableEnter: true, ApproveMode: permission.ModeAcceptEdits}),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}
	th, err := a.Thread(ctx, "plan")
	if err != nil {
		t.Fatal(err)
	}

	prompt := "Create /plan.txt containing `planned ok`. " +
		"You are in plan mode: first call exit_plan_mode with a one-line plan."
	final, _ := drain(t, th.Prompt(ctx, prompt))
	if final == nil || final.Reason != agent.ReasonInterrupted {
		t.Fatalf("final = %+v, want reason %q", final, agent.ReasonInterrupted)
	}
	intr, ok := th.Pending()
	if !ok {
		t.Fatal("the thread has no pending interrupt")
	}
	var decisions []agent.Decision
	for _, req := range intr.Requests {
		if req.Tool != "exit_plan_mode" {
			t.Errorf("pending tool = %q, want exit_plan_mode", req.Tool)
		}
		decisions = append(decisions, agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Approve})
	}
	if len(decisions) == 0 {
		t.Fatal("the interrupt lists no pending request")
	}

	final, _ = drain(t, th.Resume(ctx, decisions))
	requireCompleted(t, final)
	if got := readTrimmed(ctx, t, ws, "/plan.txt"); got != "planned ok" {
		t.Errorf("file content = %q, want %q", got, "planned ok")
	}
}

func TestLiveAsyncBackgroundTask(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	m, err := async.New(async.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	a, err := orchestrix.New(spec,
		orchestrix.WithAsync(m),
		orchestrix.WithPermissionMode(permission.ModeBypass),
		orchestrix.WithMaxTurns(12),
	)
	if err != nil {
		t.Fatal(err)
	}

	prompt := "Use start_task with the general-purpose subagent to compute 17*23 " +
		"(it should reply with just the number), then call check_task with wait true and tell me the result."
	in := orchestrix.Text(prompt)
	in.ThreadID = "async" // start_task needs a thread
	final, events := drain(t, a.Run(ctx, in))
	requireCompleted(t, final)
	for _, name := range []string{"start_task", "check_task"} {
		if !sawToolUse(events, name) {
			t.Errorf("no %s tool use was observed", name)
		}
	}
	requireTextContains(t, final.Text, "391")
}

func TestLiveCoordinatorFanOut(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	c, err := coordinator.New(spec, coordinator.Config{Workers: []subagents.Subagent{{
		Name:         "capital",
		Description:  "Finds the capital city of a country given its name.",
		SystemPrompt: "Reply with the capital city only.",
	}}},
		orchestrix.WithPermissionMode(permission.ModeBypass),
		// Planning, two start_task calls, waiting for both workers (possibly
		// over several check_task calls) and the answer need more turns than
		// the other live tests.
		orchestrix.WithMaxTurns(20),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)

	prompt := "Find the capitals of France and Japan using your capital worker, one task per country, in parallel."
	in := orchestrix.Text(prompt)
	in.ThreadID = "coordinator" // start_task needs a thread
	final, events := drain(t, c.Run(ctx, in))
	requireCompleted(t, final)
	starts := 0
	for _, ev := range events {
		if ev.Kind == agent.KindToolUse && ev.ToolName == "start_task" {
			starts++
		}
	}
	if starts < 2 {
		t.Errorf("start_task used %d times, want at least 2", starts)
	}
	requireTextContains(t, final.Text, "Paris")
	requireTextContains(t, final.Text, "Tokyo")
}

func TestLiveRubricRevision(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	a, err := orchestrix.New(spec,
		orchestrix.WithRubric(rubric.Config{Criteria: []string{"The reply ends with the exact word DONE."}}),
		orchestrix.WithPermissionMode(permission.ModeBypass),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	final, _ := drain(t, a.Run(ctx, orchestrix.Text("Say hello in one short sentence.")))
	requireCompleted(t, final)
	grades := rubric.Grades(final.State)
	if len(grades) == 0 {
		t.Fatal("no gradings were recorded")
	}
	if last := grades[len(grades)-1]; !last.Pass {
		t.Errorf("last grading = %+v, want a pass", last)
	}
	if got := strings.TrimSpace(final.Text); !strings.HasSuffix(got, "DONE") {
		t.Errorf("final text = %q, want it to end with DONE", got)
	}
}

// compressFillerChars sizes each seeded turn of the compression test so the
// conversation passes compress's trigger but not summarize's, in a window of
// 6000 tokens.
const compressFillerChars = 2000

func TestLiveCompressContext(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	mc, err := llm.ParseModel(spec)
	if err != nil {
		t.Fatal(err)
	}
	profile := llm.LookupProfile(mc.Model)
	profile.ContextWindow = 6000
	a, err := orchestrix.New(spec,
		orchestrix.WithCompress(compress.Config{}),
		orchestrix.WithSummarize(summarize.Config{TriggerFraction: 0.99}),
		orchestrix.WithConfig(func(c *agent.Config) { c.Profile = &profile }),
		orchestrix.WithPermissionMode(permission.ModeBypass),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	in := orchestrix.Text("Continue.")
	in.ThreadID = "compress" // with a thread, the originals are archived too
	const seededTurns = 8
	for i := range seededTurns {
		in.Messages = append(in.Messages,
			llm.UserText(fmt.Sprintf("Note %d:\n%s", i+1, fillerText()[:compressFillerChars])),
			llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("Noted.")}})
	}
	final, events := drain(t, a.Run(ctx, in))
	requireCompleted(t, final)
	// The elision must be the one the model's compress_context call made.
	var rangeID string
	for _, ev := range events {
		if ev.Kind == agent.KindToolUse && ev.ToolName == compress.ToolCompress {
			var input compress.CompressInput
			if err := json.Unmarshal(ev.Input, &input); err == nil {
				rangeID = input.RangeID
			}
		}
	}
	if rangeID == "" {
		t.Fatal("no compress_context tool use with a range_id was observed")
	}
	elisions := elide.All(final.State)
	found := false
	for _, e := range elisions {
		found = found || e.ID == rangeID
	}
	if !found {
		t.Errorf("elisions = %+v, want one for the compressed range %q", elisions, rangeID)
	}
}

func TestLiveMountedMemoryAcrossThreads(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	store, err := kv.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := kv.New(store, "memories")
	if err != nil {
		t.Fatal(err)
	}
	a, err := orchestrix.New(spec,
		orchestrix.WithMount(memory.DefaultDir, mounted),
		orchestrix.WithMemory(memory.Config{Store: &memory.StoreConfig{}}),
		orchestrix.WithPermissionMode(permission.ModeBypass),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	first := orchestrix.Text("Remember with record_memory that my favorite color is teal.")
	first.ThreadID = "a"
	final, events := drain(t, a.Run(ctx, first))
	requireCompleted(t, final)
	if !sawToolUse(events, "record_memory") {
		t.Error("no record_memory tool use was observed")
	}

	second := orchestrix.Text("Use recall_memory to find my favorite color, then tell me.")
	second.ThreadID = "b"
	final, events = drain(t, a.Run(ctx, second))
	requireCompleted(t, final)
	if !sawToolUse(events, "recall_memory") {
		t.Error("no recall_memory tool use was observed")
	}
	requireTextContains(t, final.Text, "teal")
}

func TestLiveProfilesPromptSuffix(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	a, err := orchestrix.New(spec,
		orchestrix.WithProfiles(profiles.Config{Adjustments: []profiles.Adjustment{{
			Models:       []string{"*"},
			PromptSuffix: "Always end your reply with the word BANANA.",
		}}}),
		orchestrix.WithPermissionMode(permission.ModeBypass),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	final, _ := drain(t, a.Run(ctx, orchestrix.Text("Say hi.")))
	requireCompleted(t, final)
	requireTextContains(t, final.Text, "BANANA")
}
