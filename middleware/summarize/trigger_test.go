package summarize

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
)

// triggerWindow is the context window of the trigger tests: the automatic
// threshold is 1700 tokens and a compaction keeps a tail of 500 tokens, about
// 2000 characters.
const triggerWindow = 2000

// triggerHistory is three messages of 2000 characters, about 1500 tokens:
// below the threshold, yet with a cut that can move once the tool loop adds
// to it.
func triggerHistory() []llm.Message {
	text := strings.Repeat("x", 2000)
	return []llm.Message{llm.UserText("A0 " + text), assistantText("A1 " + text), llm.UserText("A2 " + text)}
}

func noopCall(id string) llmtest.Call {
	return llmtest.Call{ID: id, Name: "noop", Input: map[string]any{}}
}

// toolTurn is a model turn calling noop whose request reportedly used input
// tokens.
func toolTurn(id string, input int) llmtest.Turn {
	return llmtest.Turn{ToolCalls: []llmtest.Call{noopCall(id)}, Usage: llm.Usage{Input: input}}
}

func runTrigger(main, summarizer llm.Provider, cfg Config) *agent.Final {
	cfg.Provider = summarizer
	return runWindow(main, memory.New(), cfg, agent.Input{ThreadID: "t1", Messages: triggerHistory()}, triggerWindow)
}

func TestAutoCompactAtThreshold(t *testing.T) {
	main := llmtest.New(toolTurn("n1", 1800), llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"})

	final := runTrigger(main, summarizer, Config{})

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %+v", final)
	}
	if summarizer.Calls() != 1 {
		t.Fatalf("summarizer called %d times, want 1", summarizer.Calls())
	}
	if first := main.Requests[0].Messages; len(first) != 3 || strings.Contains(first[0].Text(), "<summary>") {
		t.Fatalf("first request has %d messages, want the 3 uncompacted ones", len(first))
	}
	second := main.Requests[1].Messages
	if !strings.HasPrefix(second[0].Text(), "<summary>\nSUMMARY-1\n") {
		t.Fatalf("second request starts with %.60q, want the summary", second[0].Text())
	}
	validateView(t, second)
	if bd, _ := Current(final.State); bd.Index != 2 {
		t.Fatalf("boundary index = %d, want 2", bd.Index)
	}
}

func TestNoCompactionBelowThreshold(t *testing.T) {
	main := llmtest.New(toolTurn("n1", 1000), llmtest.Turn{Text: "done"})
	summarizer := llmtest.New()

	final := runTrigger(main, summarizer, Config{})

	if summarizer.Calls() != 0 {
		t.Fatalf("summarizer called %d times below the threshold, want 0", summarizer.Calls())
	}
	if _, ok := Current(final.State); ok {
		t.Fatal("boundary set below the threshold")
	}
}

func TestEstimateUsesObservedPlusDelta(t *testing.T) {
	s := state.New()
	s.AppendMessages(llm.UserText("hi"), assistantText("hello"))

	// Nothing observed: a pure character estimate of the view.
	view := []llm.Message{llm.UserText(strings.Repeat("x", 400))}
	if got := Estimate(s, view); got < 100 || got > 110 {
		t.Fatalf("estimate without observation = %d, want about 100", got)
	}

	// Observed tokens plus the messages added since.
	observedKey.Set(s, observed{Input: 1000, Messages: 2})
	if got := Estimate(s, view); got != 1000 {
		t.Fatalf("estimate right after observation = %d, want 1000", got)
	}
	s.AppendMessages(llm.UserText(strings.Repeat("x", 400)))
	if got := Estimate(s, view); got < 1100 || got > 1110 {
		t.Fatalf("estimate with 400 new characters = %d, want about 1100", got)
	}
}

func TestAfterModelObservesInputSideTokens(t *testing.T) {
	s := state.New()
	s.AppendMessages(llm.UserText("hi"))
	mw := New(Config{}).(agent.AfterModel)

	err := mw.AfterModel(context.Background(), s, &llm.Response{Usage: llm.Usage{Input: 10, CacheRead: 200, CacheWrite: 30, Output: 99}})

	if got, _ := observedKey.Get(s); err != nil || got != (observed{Input: 240, Messages: 1}) {
		t.Fatalf("observed = %+v, err %v, want 240 tokens at 1 message", got, err)
	}
}

func TestReactiveCompactionRetriesOnce(t *testing.T) {
	overflow := &llm.APIError{Status: 400, Body: "prompt is too long"}
	main := llmtest.New(llmtest.Turn{Err: overflow}, llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"})

	final := runTrigger(main, summarizer, Config{})

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %+v, want completed after the retry", final)
	}
	if summarizer.Calls() != 1 || main.Calls() != 2 {
		t.Fatalf("summarizer %d, main %d calls, want 1 and 2", summarizer.Calls(), main.Calls())
	}
	retry := main.Requests[1].Messages
	if !strings.HasPrefix(retry[0].Text(), "<summary>\nSUMMARY-1\n") {
		t.Fatalf("retry starts with %.60q, want the summary", retry[0].Text())
	}
	validateView(t, retry)
}

func TestReactiveCompactionGivesUpAfterOneRetry(t *testing.T) {
	overflow := &llm.APIError{Status: 400, Body: "prompt is too long"}
	main := llmtest.New(llmtest.Turn{Err: overflow}, llmtest.Turn{Err: overflow}, llmtest.Turn{Text: "never"})
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY-1"}, llmtest.Turn{Text: "SUMMARY-2"})

	final := runTrigger(main, summarizer, Config{})

	if final.Reason != agent.ReasonPromptTooLong {
		t.Fatalf("final = %+v, want prompt_too_long", final)
	}
	if summarizer.Calls() != 1 || main.Calls() != 2 {
		t.Fatalf("summarizer %d, main %d calls, want 1 and 2", summarizer.Calls(), main.Calls())
	}
}

func TestReactiveCompactionFailureReturnsOriginalError(t *testing.T) {
	overflow := &llm.APIError{Status: 400, Body: "prompt is too long"}
	main := llmtest.New(llmtest.Turn{Err: overflow})
	summarizer := llmtest.New(llmtest.Turn{Err: errors.New("summarizer down")})

	final := runTrigger(main, summarizer, Config{})

	if final.Reason != agent.ReasonPromptTooLong || !errors.Is(final.Err, overflow) {
		t.Fatalf("final = %+v, want prompt_too_long wrapping the original error", final)
	}
}

func TestCircuitBreakerOpensAfterFailures(t *testing.T) {
	main := llmtest.New(
		toolTurn("n1", 1800), toolTurn("n2", 1800), toolTurn("n3", 1800),
		toolTurn("n4", 1800), toolTurn("n5", 1800), llmtest.Turn{Text: "done"},
	)
	down := llmtest.Turn{Err: errors.New("summarizer down")}
	summarizer := llmtest.New(down, down, down, down, down)

	final := runTrigger(main, summarizer, Config{})

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %+v, want the run to complete without compaction", final)
	}
	if summarizer.Calls() != 3 {
		t.Fatalf("summarizer called %d times, want exactly MaxFailures (3)", summarizer.Calls())
	}
	bd, _ := BoundaryKey.Get(final.State)
	if !strings.Contains(bd.LastError, "stopped after 3 failures") || !strings.Contains(bd.LastError, "summarizer down") {
		t.Fatalf("LastError = %q, want the open breaker and the cause", bd.LastError)
	}
}

func TestBreakerResetsOnSuccessfulCompaction(t *testing.T) {
	main := llmtest.New(toolTurn("n1", 1800), toolTurn("n2", 1800), toolTurn("n3", 100), llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Err: errors.New("down")}, llmtest.Turn{Text: "SUMMARY-1"})

	final := runTrigger(main, summarizer, Config{})

	if failures, _ := failuresKey.Get(final.State); failures != 0 {
		t.Fatalf("failures = %d after a successful compaction, want 0", failures)
	}
	if bd, ok := Current(final.State); !ok || bd.LastError != "" {
		t.Fatalf("boundary = %+v, want a compaction without error", bd)
	}
}

func TestNoProgressCountsAsFailure(t *testing.T) {
	// One message alone is over the threshold, but there is nothing before a
	// tail to summarize: the trigger must not loop, it must count a failure.
	main := llmtest.New(llmtest.Turn{Text: "done"})
	summarizer := llmtest.New()
	huge := []llm.Message{llm.UserText(strings.Repeat("x", 8000))}

	final := runWindow(main, memory.New(), Config{Provider: summarizer, MaxFailures: 1}, agent.Input{Messages: huge}, triggerWindow)

	if final.Reason != agent.ReasonCompleted || summarizer.Calls() != 0 {
		t.Fatalf("final = %+v, summarizer calls %d, want completion without summarizing", final, summarizer.Calls())
	}
	if failures, _ := failuresKey.Get(final.State); failures != 1 {
		t.Fatalf("failures = %d, want 1", failures)
	}
	if bd, _ := BoundaryKey.Get(final.State); !strings.Contains(bd.LastError, "no progress") {
		t.Fatalf("LastError = %q, want the no-progress failure", bd.LastError)
	}
}

func TestNewRunClosesBreaker(t *testing.T) {
	s := state.New()
	failuresKey.Set(s, DefaultMaxFailures)

	if err := New(Config{}).(agent.BeforeAgent).BeforeAgent(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, ok := failuresKey.Get(s); ok {
		t.Fatal("failure count survived the start of a run")
	}
}
