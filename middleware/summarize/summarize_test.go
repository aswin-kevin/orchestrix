package summarize

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// testWindow is the context window of the test agents. With
// DefaultKeepFraction it keeps a tail of 1000 tokens, about 4000 characters,
// so each padded message below (3000+ characters) moves the cut by one.
const testWindow = 4000

// noAutoTrigger is a TriggerFraction that never fires for the padded
// history, which stays below the whole window, so tests of the
// compact_conversation tool see only the compactions they ask for.
const noAutoTrigger = 1

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
	t.Helper()
	a, err := agent.New(agent.Config{
		Provider:       main,
		Profile:        &llm.Profile{ContextWindow: window, MaxOutput: 500},
		Backend:        b,
		Tools:          []tool.Tool{noopTool()},
		Middleware:     []agent.Middleware{mustNew(t, cfg)},
		PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	var final *agent.Final
	for ev := range a.Run(t.Context(), in) {
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	return final
}

func readFile(t *testing.T, b backend.Backend, path string) string {
	t.Helper()
	res, err := b.Read(t.Context(), path, 0, 1_000_000)
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
	b := testutil.Memory(t)

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
		// Alone longer than the kept tail, so the second cut lands right
		// before it.
		llmtest.Turn{Text: "B " + strings.Repeat("x", 5000), ToolCalls: []llmtest.Call{compactCall("c2")}},
		llmtest.Turn{Text: "done"},
	)
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"}, llmtest.Turn{Text: "SUMMARY-2"})
	b := testutil.Memory(t)

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

	final := runAgent(t, main, testutil.Memory(t), Config{Provider: summarizer}, []llm.Message{llm.UserText("hi")})

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
	b := testutil.Memory(t)

	final := runAgent(t, main, b, Config{DisableOffload: true, TriggerFraction: noAutoTrigger}, paddedHistory())

	if main.Calls() != 3 || main.Requests[1].System[0] != DefaultSummaryPrompt {
		t.Fatalf("main provider got %d requests, want the summarizer call second", main.Calls())
	}
	if bd, _ := Current(final.State); bd.Summary != "SUMMARY-1" || bd.Path != "" {
		t.Fatalf("boundary = %+v, want SUMMARY-1 without a path", bd)
	}
	if _, err := b.Read(t.Context(), HistoryDir+"/t1/1.md", 0, 1); err == nil {
		t.Fatal("transcript written although offload is disabled")
	}
}

func TestThreadlessRunSkipsOffload(t *testing.T) {
	main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}}, llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"})
	b := testutil.Memory(t)

	final := runInput(t, main, b, Config{Provider: summarizer}, agent.Input{Messages: paddedHistory()})

	if bd, ok := Current(final.State); !ok || bd.Summary != "SUMMARY-1" || bd.Path != "" {
		t.Fatalf("boundary = %+v, want SUMMARY-1 without a path", bd)
	}
	if files, _ := b.Glob(t.Context(), "**", HistoryDir); len(files) != 0 {
		t.Fatalf("threadless run wrote transcripts: %+v", files)
	}
}

func TestFailedCompactionRecordedAndRequestUncompacted(t *testing.T) {
	cfg := Config{TriggerFraction: noAutoTrigger}
	main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}}, llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Err: errors.New("summarizer down")})

	cfg.Provider = summarizer
	final := runAgent(t, main, testutil.Memory(t), cfg, paddedHistory())

	if summarizer.Calls() != 1 {
		t.Fatalf("summarizer called %d times, want 1", summarizer.Calls())
	}
	if _, ok := Current(final.State); ok {
		t.Fatal("a failed compaction must not set a boundary")
	}
	if failures, _ := failuresKey.Get(final.State); failures != 1 {
		t.Fatalf("failures = %d, want the requested compaction's summarizer error counted", failures)
	}
	bd, _ := boundaryKey.Get(final.State)
	if want := "summarize conversation: summarizer down"; bd.LastError != want {
		t.Fatalf("LastError = %q, want %q", bd.LastError, want)
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

	final := runAgent(t, main, testutil.Memory(t), Config{Provider: summarizer}, paddedHistory())

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

	final := runAgent(t, main, testutil.Memory(t), Config{Provider: summarizer}, paddedHistory())

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

func TestHistoryDirIsUnderTheInternalRoot(t *testing.T) {
	if HistoryDir != "/.orchestrix/conversation_history" {
		t.Fatalf("HistoryDir = %q, want it under /.orchestrix", HistoryDir)
	}
}

func TestName(t *testing.T) {
	if Name != "summarize" {
		t.Fatalf("Name = %q, want summarize", Name)
	}
	if got := mustNew(t, Config{}).Name(); got != Name {
		t.Fatalf("Name() = %q, want %q", got, Name)
	}
}

// recordingSummarizer answers with replies in turn and records every request.
type recordingSummarizer struct {
	replies  []string
	requests []SummaryRequest
}

func (r *recordingSummarizer) Summarize(_ context.Context, req SummaryRequest) (string, error) {
	r.requests = append(r.requests, req)
	reply := r.replies[0]
	r.replies = r.replies[1:]
	return reply, nil
}

func TestCustomSummarizerUsed(t *testing.T) {
	main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}}, llmtest.Turn{Text: "done"})
	var got []SummaryRequest
	s := SummarizerFunc(func(_ context.Context, req SummaryRequest) (string, error) {
		got = append(got, req)
		return " S\n", nil
	})

	final := runAgent(t, main, testutil.Memory(t), Config{Summarizer: s}, paddedHistory())

	if bd, ok := Current(final.State); !ok || bd.Summary != "S" || bd.Index != 3 {
		t.Fatalf("boundary = %+v (set %v), want the trimmed custom summary at index 3", bd, ok)
	}
	if main.Calls() != 2 {
		t.Fatalf("main provider got %d requests, want only the 2 agent turns", main.Calls())
	}
	if len(got) != 1 {
		t.Fatalf("summarizer called %d times, want 1", len(got))
	}
	req := got[0]
	if req.Previous != "" || len(req.Messages) != 3 || !strings.HasPrefix(req.Messages[0].Text(), "A0 ") {
		t.Fatalf("request = previous %q, %d messages, want none and A0..A2", req.Previous, len(req.Messages))
	}
	if req.Transcript != boundedRender(req.Messages, maxSummaryInputChars) {
		t.Fatal("Transcript is not the bounded rendering of Messages")
	}
}

func TestSummarizerSeesPrevious(t *testing.T) {
	main := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}},
		llmtest.Turn{Text: "B " + strings.Repeat("x", 5000), ToolCalls: []llmtest.Call{compactCall("c2")}},
		llmtest.Turn{Text: "done"},
	)
	rec := &recordingSummarizer{replies: []string{"S1", "S2"}}

	final := runAgent(t, main, testutil.Memory(t), Config{Summarizer: rec}, paddedHistory())

	if len(rec.requests) != 2 {
		t.Fatalf("summarizer called %d times, want 2", len(rec.requests))
	}
	second := rec.requests[1]
	if second.Previous != "S1" || len(second.Messages) == 0 || !strings.HasPrefix(second.Messages[0].Text(), "A3 ") {
		t.Fatalf("second request = previous %q, %d messages, want S1 and the messages from A3", second.Previous, len(second.Messages))
	}
	if !strings.Contains(second.Transcript, "A3 ") || strings.Contains(second.Transcript, "A0 ") {
		t.Fatal("second transcript does not cover exactly the messages after the first boundary")
	}
	if bd, _ := Current(final.State); bd.Summary != "S2" || bd.Index != 7 {
		t.Fatalf("boundary = %+v, want S2 at index 7", bd)
	}
}

func TestCustomSummarizerFailureRecorded(t *testing.T) {
	for name, s := range map[string]Summarizer{
		"error": SummarizerFunc(func(context.Context, SummaryRequest) (string, error) { return "", errors.New("custom down") }),
		"blank": SummarizerFunc(func(context.Context, SummaryRequest) (string, error) { return " \n", nil }),
	} {
		t.Run(name, func(t *testing.T) {
			main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}}, llmtest.Turn{Text: "done"})

			final := runAgent(t, main, testutil.Memory(t), Config{Summarizer: s, TriggerFraction: noAutoTrigger}, paddedHistory())

			if _, ok := Current(final.State); ok {
				t.Fatal("a failed compaction set a boundary")
			}
			bd, _ := boundaryKey.Get(final.State)
			if want := map[string]string{"error": "summarize conversation: custom down", "blank": "summarize conversation: summarizer returned no text"}[name]; bd.LastError != want {
				t.Fatalf("LastError = %q, want %q", bd.LastError, want)
			}
			if failures, _ := failuresKey.Get(final.State); failures != 1 {
				t.Fatalf("failures = %d, want 1", failures)
			}
		})
	}
}

func TestSummaryPromptEmptyMeansDefault(t *testing.T) {
	req := SummaryRequest{Messages: []llm.Message{llm.UserText("hi")}, Previous: "P", Transcript: "T"}
	for _, tt := range []struct{ prompt, want string }{{"", DefaultSummaryPrompt}, {"custom", "custom"}} {
		p := llmtest.New(llmtest.Turn{Text: " SUMMARY \n"})
		got, err := (&ModelSummarizer{Provider: p, Model: "m", Prompt: tt.prompt}).Summarize(t.Context(), req)
		if err != nil || got != "SUMMARY" {
			t.Fatalf("Summarize = %q, %v, want the trimmed reply", got, err)
		}
		sent := p.Requests[0]
		if len(sent.System) != 1 || sent.System[0] != tt.want || sent.Model != "m" || sent.MaxTokens != DefaultSummaryMaxTokens {
			t.Fatalf("prompt %q: request = %+v, want system %.20q", tt.prompt, sent, tt.want)
		}
		if len(sent.Messages) != 1 || sent.Messages[0].Text() != "Previous summary:\nP\n\nNew conversation:\nT" {
			t.Fatalf("request messages = %+v", sent.Messages)
		}
	}
}

func TestSummaryPromptReachesDefaultSummarizer(t *testing.T) {
	main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}}, llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"})

	runAgent(t, main, testutil.Memory(t), Config{Provider: summarizer, Model: "small", SummaryPrompt: "custom"}, paddedHistory())

	if req := summarizer.Requests[0]; req.System[0] != "custom" || req.Model != "small" {
		t.Fatalf("summarizer request system %q, model %q, want custom and small", req.System[0], req.Model)
	}
}

func TestModelSummarizerFallsBackToTheRunsAgent(t *testing.T) {
	// The run's own provider answers the summarizer call as its second
	// request.
	main := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("c1")}},
		llmtest.Turn{Text: "SUMMARY-1"},
		llmtest.Turn{Text: "done"},
	)

	final := runAgent(t, main, testutil.Memory(t), Config{Summarizer: &ModelSummarizer{}, TriggerFraction: noAutoTrigger}, paddedHistory())

	if main.Calls() != 3 || main.Requests[1].System[0] != DefaultSummaryPrompt {
		t.Fatalf("main provider got %d requests, want the summarizer call second", main.Calls())
	}
	if bd, _ := Current(final.State); bd.Summary != "SUMMARY-1" {
		t.Fatalf("boundary = %+v, want SUMMARY-1", bd)
	}
}

func TestModelSummarizerOutsideARunNeedsAProvider(t *testing.T) {
	if _, err := (&ModelSummarizer{}).Summarize(t.Context(), SummaryRequest{}); err == nil || !strings.HasPrefix(err.Error(), "summarize: ") {
		t.Fatalf("err = %v, want a summarize: error", err)
	}
}

func TestNewRejectsBadFractions(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"zero", Config{}, true},
		{"trigger one is legal", Config{TriggerFraction: 1}, true},
		{"keep just below one", Config{KeepFraction: 0.99}, true},
		{"trigger negative", Config{TriggerFraction: -0.1}, false},
		{"trigger above one", Config{TriggerFraction: 1.5}, false},
		{"trigger NaN", Config{TriggerFraction: math.NaN()}, false},
		{"keep negative", Config{KeepFraction: -0.1}, false},
		{"keep one", Config{KeepFraction: 1}, false},
		{"keep NaN", Config{KeepFraction: math.NaN()}, false},
		{"KeepToolResults negative", Config{KeepToolResults: -1}, false},
		{"ClearResultChars negative", Config{ClearResultChars: -1}, false},
		{"MaxOldInputChars negative", Config{MaxOldInputChars: -1}, false},
		{"MaxFailures negative", Config{MaxFailures: -1}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, err := New(tt.cfg)
			if tt.ok != (err == nil) || tt.ok != (m != nil) {
				t.Fatalf("New = %v, %v; want ok %v", m, err, tt.ok)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "summarize: ") {
				t.Fatalf("error %q lacks the summarize: prefix", err)
			}
		})
	}
}

func TestSummarizerExcludesModelFields(t *testing.T) {
	s := SummarizerFunc(func(context.Context, SummaryRequest) (string, error) { return "S", nil })
	for name, cfg := range map[string]Config{
		"provider": {Summarizer: s, Provider: llmtest.New()},
		"model":    {Summarizer: s, Model: "m"},
		"prompt":   {Summarizer: s, SummaryPrompt: "p"},
	} {
		_, err := New(cfg)
		if err == nil || err.Error() != "summarize: Summarizer excludes Provider, Model and SummaryPrompt" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := New(Config{Summarizer: s}); err != nil {
		t.Fatalf("Summarizer alone: %v", err)
	}
}
