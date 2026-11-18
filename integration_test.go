package orchestrix_test

import (
	"context"
	"iter"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	checkpointfile "github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
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

	ws := memory.New()
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

// drain drains a run, failing the test on a stream error, and returns its
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

// readTrimmed returns the content of path in ws without surrounding space.
func readTrimmed(t *testing.T, ctx context.Context, ws *memory.Backend, path string) string {
	t.Helper()
	got, err := ws.Read(ctx, path, 0, 10)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.TrimSpace(strings.Join(got.Lines, "\n"))
}

func TestLiveSubagentDelegation(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	ws := memory.New()
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
	if got := readTrimmed(t, ctx, ws, "/report.txt"); got != "delegated ok" {
		t.Errorf("file content = %q, want %q", got, "delegated ok")
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
	probe, _ := drain(t, minimalSummarizingAgent(t, spec, llm.DefaultProfile.ContextWindow).
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

	if b, ok := summarize.Current(thread.State()); !ok {
		t.Errorf("no compaction happened (boundary %+v)", b)
	}
	if !strings.Contains(strings.ToUpper(final.Text), "ORCHID") {
		t.Errorf("final text = %q, want it to mention ORCHID", final.Text)
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

	second, err := newAgent().Thread(ctx, "live")
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
	if !strings.Contains(final.Text, "resume ok") {
		t.Errorf("final text = %q, want it to contain %q", final.Text, "resume ok")
	}
}
