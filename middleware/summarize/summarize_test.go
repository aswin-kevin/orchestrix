package summarize

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// testWindow is the context window of the test agents. With
// DefaultKeepFraction it keeps a tail of 1000 tokens, about 4000 characters,
// so each padded message below (3000+ characters) moves the cut by one.
const testWindow = 4000

// noAutoTrigger is a TriggerFraction that never fires, so tests of the
// compact_conversation tool see only the compactions they ask for.
const noAutoTrigger = 100

// padded returns a message text starting with marker and long enough that a
// few of them exceed the kept tail.
func padded(marker string) string {
	return marker + " " + strings.Repeat("x", 3000)
}

// paddedHistory is the conversation before the model's first turn: four
// padded messages A0..A3 and a padded last user message A4.
func paddedHistory() []llm.Message {
	return []llm.Message{
		llm.UserText(padded("A0")), assistantText(padded("A1")),
		llm.UserText(padded("A2")), assistantText(padded("A3")),
		llm.UserText(padded("A4")),
	}
}

func compactCall(id string) llmtest.Call {
	return llmtest.Call{ID: id, Name: ToolCompact, Input: map[string]any{}}
}

func runAgent(t *testing.T, main llm.Provider, b backend.Backend, cfg Config, msgs []llm.Message) *agent.Final {
	t.Helper()
	return runInput(t, main, b, cfg, agent.Input{ThreadID: "t1", Messages: msgs})
}

// noopTool answers every call with "ok".
func noopTool() tool.Tool {
	return tool.New("noop", "Does nothing.", func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
		return tool.Text("ok"), nil
	}, tool.WithReadOnly(), tool.WithConcurrent())
}

func runInput(t *testing.T, main llm.Provider, b backend.Backend, cfg Config, in agent.Input) *agent.Final {
	t.Helper()
	final := runWindow(t, main, b, cfg, in, testWindow)
	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Fatalf("run did not complete: %+v", final)
	}
	return final
}

// runWindow runs the test agent with the given context window and returns
// its Final whatever the reason.
func runWindow(t *testing.T, main llm.Provider, b backend.Backend, cfg Config, in agent.Input, window int) *agent.Final {
	a, err := agent.New(agent.Config{
		Provider:       main,
		Profile:        &llm.Profile{ContextWindow: window, MaxOutput: 500},
		Backend:        b,
		Tools:          []tool.Tool{noopTool()},
		Middleware:     []agent.Middleware{New(cfg)},
		PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	var final *agent.Final
	for ev := range a.Run(context.Background(), in) {
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	return final
}

func readFile(t *testing.T, b backend.Backend, path string) string {
	t.Helper()
	res, err := b.Read(context.Background(), path, 0, 1_000_000)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Join(res.Lines, "\n")
}

func summarizerInput(t *testing.T, p *llmtest.Scripted, i int) string {
	t.Helper()
	req := p.Requests[i]
	if len(req.Messages) != 1 || len(req.System) != 1 || req.System[0] != DefaultSummaryPrompt || req.MaxTokens != DefaultSummaryMaxTokens {
		t.Fatalf("summarizer request %d = %+v, want the summary prompt and one user message", i, req)
	}
	return req.Messages[0].Text()
}

func TestCompactToolSummarizesAndOffloads(t *testing.T) {
	main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}}, llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"})
	b := memory.New()

	final := runAgent(t, main, b, Config{Provider: summarizer}, paddedHistory())

	if summarizer.Calls() != 1 {
		t.Fatalf("summarizer called %d times, want 1", summarizer.Calls())
	}
	// The tail keeps the compact call, its result, A4 and A3 (>= 4000 chars),
	// so A0..A2 are summarized.
	input := summarizerInput(t, summarizer, 0)
	for _, want := range []string{"Previous summary:\n\n\nNew conversation:\n", "## user\nA0 ", "## assistant\nA1 ", "## user\nA2 "} {
		if !strings.Contains(input, want) {
			t.Errorf("summarizer input missing %q", want)
		}
	}
	if strings.Contains(input, "A3 ") {
		t.Error("summarizer input contains A3, which belongs to the kept tail")
	}

	path := HistoryDir + "/t1/1.md"
	b2, ok := Current(final.State)
	if !ok || b2.Index != 3 || b2.Summary != "SUMMARY-1" || b2.Path != path {
		t.Fatalf("boundary = %+v (set %v), want index 3, SUMMARY-1, %s", b2, ok, path)
	}
	file := readFile(t, b, path)
	for _, want := range []string{"A0 ", "A1 ", "A2 ", strings.Repeat("x", 3000)} {
		if !strings.Contains(file, want) {
			t.Errorf("offloaded transcript missing %.20q", want)
		}
	}

	next := main.Requests[1].Messages
	if !strings.HasPrefix(next[0].Text(), "<summary>\nSUMMARY-1\n</summary>") || !strings.HasSuffix(next[0].Text(), path+".") {
		t.Fatalf("next request starts with %q, want the summary message", next[0].Text())
	}
	if len(next) != 5 || !strings.HasPrefix(next[1].Text(), "A3 ") {
		t.Fatalf("next request has %d messages, want summary, A3, A4, call, result", len(next))
	}
	validateView(t, next)
	if got := len(final.State.Messages()); got != 8 {
		t.Fatalf("state holds %d messages, want the complete 8", got)
	}
}

func TestIncrementalSummaryIncludesPrevious(t *testing.T) {
	main := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}},
		// Alone longer than the kept tail, so the second cut lands right before it.
		llmtest.Turn{Text: "B " + strings.Repeat("x", 5000), ToolCalls: []llmtest.Call{compactCall("c2")}},
		llmtest.Turn{Text: "done"},
	)
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"}, llmtest.Turn{Text: "SUMMARY-2"})
	b := memory.New()

	final := runAgent(t, main, b, Config{Provider: summarizer}, paddedHistory())

	if summarizer.Calls() != 2 {
		t.Fatalf("summarizer called %d times, want 2", summarizer.Calls())
	}
	second := summarizerInput(t, summarizer, 1)
	if !strings.HasPrefix(second, "Previous summary:\nSUMMARY-1\n\nNew conversation:\n") {
		t.Fatalf("second summarizer input starts %.80q, want the first summary", second)
	}
	if !strings.Contains(second, "A3 ") || !strings.Contains(second, "A4 ") || strings.Contains(second, "A0 ") {
		t.Error("second compaction must cover exactly the messages after the first boundary")
	}
	if !strings.Contains(second, "tool_call "+ToolCompact+" {}") || !strings.Contains(second, "tool_result c1: ") {
		t.Errorf("second summarizer input does not render the tool turn: %.300q", second[len(second)-300:])
	}

	if bd, _ := Current(final.State); bd.Index != 7 || bd.Summary != "SUMMARY-2" || bd.Path != HistoryDir+"/t1/2.md" {
		t.Fatalf("boundary = %+v, want index 7, SUMMARY-2, file 2", bd)
	}
	if first := readFile(t, b, HistoryDir+"/t1/1.md"); !strings.Contains(first, "A0 ") || strings.Contains(first, "A3 ") {
		t.Error("first transcript file was changed by the second compaction")
	}
	if !strings.Contains(readFile(t, b, HistoryDir+"/t1/2.md"), "A3 ") {
		t.Error("second transcript file misses its messages")
	}
	if last := main.Requests[2].Messages[0].Text(); !strings.HasPrefix(last, "<summary>\nSUMMARY-2\n") {
		t.Fatalf("last request starts with %q, want the second summary", last)
	}
}

func TestCompactWithNothingToSummarize(t *testing.T) {
	main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}}, llmtest.Turn{Text: "done"})
	summarizer := llmtest.New()

	final := runAgent(t, main, memory.New(), Config{Provider: summarizer}, []llm.Message{llm.UserText("hi")})

	if summarizer.Calls() != 0 {
		t.Fatalf("summarizer called %d times for a short conversation, want 0", summarizer.Calls())
	}
	if _, ok := Current(final.State); ok {
		t.Fatal("boundary set although nothing was summarized")
	}
	if _, ok := requestedKey.Get(final.State); ok {
		t.Fatal("compaction request flag left set")
	}
	if _, ok := failuresKey.Get(final.State); ok {
		t.Fatal("a requested compaction with nothing to summarize counted as a failure")
	}
}

func TestDisableOffloadAndDefaultSummarizer(t *testing.T) {
	// No Config.Provider: the request's own provider summarizes, so its
	// second request is the summarizer call.
	main := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}},
		llmtest.Turn{Text: "SUMMARY-1"},
		llmtest.Turn{Text: "done"},
	)
	b := memory.New()

	final := runAgent(t, main, b, Config{DisableOffload: true, TriggerFraction: noAutoTrigger}, paddedHistory())

	if main.Calls() != 3 || main.Requests[1].System[0] != DefaultSummaryPrompt {
		t.Fatalf("main provider got %d requests, want the summarizer call second", main.Calls())
	}
	if bd, _ := Current(final.State); bd.Summary != "SUMMARY-1" || bd.Path != "" {
		t.Fatalf("boundary = %+v, want SUMMARY-1 without a path", bd)
	}
	if _, err := b.Read(context.Background(), HistoryDir+"/t1/1.md", 0, 1); err == nil {
		t.Fatal("transcript written although offload is disabled")
	}
}

func TestThreadlessRunSkipsOffload(t *testing.T) {
	main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}}, llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"})
	b := memory.New()

	final := runInput(t, main, b, Config{Provider: summarizer}, agent.Input{Messages: paddedHistory()})

	if bd, ok := Current(final.State); !ok || bd.Summary != "SUMMARY-1" || bd.Path != "" {
		t.Fatalf("boundary = %+v, want SUMMARY-1 without a path", bd)
	}
	if files, _ := b.Glob(context.Background(), "**", HistoryDir); len(files) != 0 {
		t.Fatalf("threadless run wrote transcripts: %+v", files)
	}
}

func TestFailedCompactionRecordedAndRequestUncompacted(t *testing.T) {
	cfg := Config{TriggerFraction: noAutoTrigger}
	main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}}, llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Err: errors.New("summarizer down")})

	cfg.Provider = summarizer
	final := runAgent(t, main, memory.New(), cfg, paddedHistory())

	if summarizer.Calls() != 1 {
		t.Fatalf("summarizer called %d times, want 1", summarizer.Calls())
	}
	if _, ok := Current(final.State); ok {
		t.Fatal("a failed compaction must not set a boundary")
	}
	if failures, _ := failuresKey.Get(final.State); failures != 1 {
		t.Fatalf("failures = %d, want the requested compaction's summarizer error counted", failures)
	}
	bd, _ := BoundaryKey.Get(final.State)
	if !strings.Contains(bd.LastError, "summarizer down") {
		t.Fatalf("LastError = %q, want the summarizer error", bd.LastError)
	}
	if next := main.Requests[1].Messages; len(next) != 7 || strings.Contains(next[0].Text(), "<summary>") {
		t.Fatalf("next request has %d messages, want the uncompacted 7", len(next))
	}
}

func TestSuccessfulCompactionClearsLastError(t *testing.T) {
	main := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}},
		llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c2")}},
		llmtest.Turn{Text: "done"},
	)
	summarizer := llmtest.New(llmtest.Turn{Err: errors.New("summarizer down")}, llmtest.Turn{Text: "SUMMARY-1"})

	final := runAgent(t, main, memory.New(), Config{Provider: summarizer}, paddedHistory())

	if bd, ok := Current(final.State); !ok || bd.Summary != "SUMMARY-1" || bd.LastError != "" || bd.Compactions != 1 {
		t.Fatalf("boundary = %+v, want SUMMARY-1, compaction 1, no error", bd)
	}
}

func TestCompactAlongsideParallelCalls(t *testing.T) {
	main := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{
			{ID: "n1", Name: "noop", Input: map[string]any{}}, compactCall("c1"), {ID: "n2", Name: "noop", Input: map[string]any{}},
		}},
		llmtest.Turn{Text: "done"},
	)
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"})

	final := runAgent(t, main, memory.New(), Config{Provider: summarizer}, paddedHistory())

	if summarizer.Calls() != 1 {
		t.Fatalf("summarizer called %d times, want 1", summarizer.Calls())
	}
	if bd, _ := Current(final.State); bd.Index != 3 {
		t.Fatalf("boundary index = %d, want 3", bd.Index)
	}
	next := main.Requests[1].Messages
	validateView(t, next)
	if results := next[len(next)-1].Content; len(results) != 3 {
		t.Fatalf("last message holds %d results, want all 3 parallel results", len(results))
	}
}
