package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

const settlementPrompt = "Limit reached. Summarise where you got to."

func echoCall(id string) llmtest.Call {
	return llmtest.Call{ID: id, Name: "echo", Input: map[string]any{"text": id}}
}

func toolTurn(id string) llmtest.Turn {
	return llmtest.Turn{ToolCalls: []llmtest.Call{echoCall(id)}}
}

// schemaNames lists the tool names offered by req.
func schemaNames(req llm.Request) []string {
	var names []string
	for _, s := range req.Tools {
		names = append(names, s.Name)
	}
	return names
}

// lastText returns the text of the last message of req.
func lastText(req llm.Request) string {
	return req.Messages[len(req.Messages)-1].Text()
}

func TestMaxTurnsWithoutSettlement(t *testing.T) {
	p := llmtest.New(toolTurn("t1"), toolTurn("t2"), toolTurn("t3"))
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{echoTool()}, MaxTurns: 2})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonMaxTurns || final.Err != nil || final.Turns != 2 {
		t.Fatalf("final = %+v, want max_turns after 2 turns", final)
	}
	if p.Calls() != 2 {
		t.Fatalf("model called %d times, want 2", p.Calls())
	}
	// The tools of the last allowed turn still ran, so State stays paired.
	if res := lastToolResults(t, final.State); res[0].ToolUseID != "t2" || res[0].IsError {
		t.Fatalf("last results = %+v, want echo result for t2", res)
	}
}

func TestMaxTurnsSettlementHidesToolsAndKeepsReason(t *testing.T) {
	var executed bool
	execute := tool.New("execute", "Run a command.",
		func(context.Context, echoInput, *tool.Call) (tool.Result, error) {
			executed = true
			return tool.Text("ran"), nil
		})
	p := llmtest.New(
		toolTurn("t1"),
		// The model may still name a hidden tool during settlement.
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "x1", Name: "execute", Input: map[string]any{"text": "rm"}}}},
		toolTurn("t3"),
	)
	a := mustNew(t, Config{
		Provider: p,
		Tools:    []tool.Tool{echoTool(), execute},
		MaxTurns: 1,
		Settlement: &Settlement{
			Prompt:        settlementPrompt,
			MaxTurns:      2,
			DisabledTools: []string{"execute"},
		},
	})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonMaxTurns || final.Err != nil {
		t.Fatalf("final = %+v, want max_turns", final)
	}
	if p.Calls() != 3 {
		t.Fatalf("model called %d times, want 1 + 2 settlement turns", p.Calls())
	}
	if got := schemaNames(p.Requests[0]); !slices.Contains(got, "execute") {
		t.Fatalf("first request tools = %v, want execute offered", got)
	}
	if got := lastText(p.Requests[1]); got != settlementPrompt {
		t.Fatalf("first settlement request ends with %q, want the settlement prompt", got)
	}
	for _, req := range p.Requests[1:] {
		if got := schemaNames(req); slices.Contains(got, "execute") || !slices.Contains(got, "echo") {
			t.Fatalf("settlement request tools = %v, want echo without execute", got)
		}
	}
	if executed {
		t.Fatal("a disabled tool ran during settlement")
	}
	refused := p.Requests[2].Messages[len(p.Requests[2].Messages)-1].Content[0]
	if refused.ToolUseID != "x1" || !refused.IsError {
		t.Fatalf("disabled call result = %+v, want an error result", refused)
	}
}

func TestSettlementNaturalStopUsesPromptByReason(t *testing.T) {
	const byReason = "Out of turns: wrap up."
	p := llmtest.New(toolTurn("t1"), llmtest.Turn{Text: "summary"})
	a := mustNew(t, Config{
		Provider: p,
		Tools:    []tool.Tool{echoTool()},
		MaxTurns: 1,
		Settlement: &Settlement{
			Prompt:         settlementPrompt,
			PromptByReason: map[TerminalReason]string{ReasonMaxTurns: byReason},
		},
	})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonMaxTurns || final.Text != "summary" {
		t.Fatalf("final = %+v, want max_turns with the summary", final)
	}
	if got := lastText(p.Requests[1]); got != byReason {
		t.Fatalf("settlement prompt = %q, want the per-reason override", got)
	}
}

// hangTool blocks until its context ends, failing the test if that takes
// longer than limit.
func hangTool(t *testing.T, limit time.Duration) tool.Tool {
	return tool.New("hang", "Block until interrupted.",
		func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
			select {
			case <-ctx.Done():
				return tool.Result{}, ctx.Err()
			case <-time.After(limit):
				t.Error("hung tool was never interrupted")
				return tool.Text("timed out"), nil
			}
		},
		tool.WithReadOnly())
}

func TestMaxDurationInterruptsHungTool(t *testing.T) {
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "h1", Name: "hang"}}})
	a := mustNew(t, Config{
		Provider:    p,
		Tools:       []tool.Tool{hangTool(t, 5*time.Second)},
		MaxDuration: 50 * time.Millisecond,
	})

	start := time.Now()
	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("run took %v, want it interrupted near the 50ms budget", elapsed)
	}
	if final.Reason != ReasonTimeout || final.Err != nil {
		t.Fatalf("final = %+v, want timeout", final)
	}
	res := lastToolResults(t, final.State)[0]
	if !res.IsError || !strings.Contains(res.Content[0].Text, "time budget") {
		t.Fatalf("hung tool result = %+v, want a time budget interruption", res)
	}
}

func TestTimeoutSettlementRunsOnParentContext(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "h1", Name: "hang"}}},
		llmtest.Turn{Text: "summary"},
	)
	a := mustNew(t, Config{
		Provider:    p,
		Tools:       []tool.Tool{hangTool(t, 5*time.Second)},
		MaxDuration: 50 * time.Millisecond,
		Settlement:  &Settlement{Prompt: settlementPrompt},
	})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	// The settlement model call would fail if it ran on the expired deadline.
	if final.Reason != ReasonTimeout || final.Text != "summary" || final.Err != nil {
		t.Fatalf("final = %+v, want timeout with the settlement summary", final)
	}
	if got := lastText(p.Requests[1]); got != settlementPrompt {
		t.Fatalf("settlement request ends with %q, want the settlement prompt", got)
	}
}

func textTurn(text string, output int) llmtest.Turn {
	return llmtest.Turn{Text: text, Usage: llm.Usage{Output: output}}
}

func TestTokenBudgetNudgesThenStops(t *testing.T) {
	p := llmtest.New(textTurn("a", 100), textTurn("b", 100), textTurn("c", 100), textTurn("never", 1))
	a := mustNew(t, Config{Provider: p, TokenBudget: 10_000})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	// Both nudged turns produced fewer than 500 tokens, so the third reply
	// ends the run.
	if final.Reason != ReasonCompleted || final.Text != "c" || p.Calls() != 3 {
		t.Fatalf("final = %+v after %d calls, want completed after 3", final, p.Calls())
	}
	for i, remaining := range []int{9_900, 9_800} {
		want := fmt.Sprintf(defaultBudgetNudge, remaining)
		if got := lastText(p.Requests[i+1]); got != want {
			t.Fatalf("request %d ends with %q, want %q", i+1, got, want)
		}
	}
}

func TestTokenBudgetStopsNearBudget(t *testing.T) {
	p := llmtest.New(textTurn("a", 950), textTurn("never", 1))
	a := mustNew(t, Config{Provider: p, TokenBudget: 1_000})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted || p.Calls() != 1 {
		t.Fatalf("final = %+v after %d calls, want completed without a nudge", final, p.Calls())
	}
}

func truncatedTurn(text string) llmtest.Turn {
	return llmtest.Turn{Text: text, StopReason: llm.StopMaxTokens}
}

func TestMaxTokensEscalateThenResume(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxOutput int
		want      int
	}{
		{"escalates to the cap", 100_000, DefaultEscalatedMaxTokens},
		{"capped at the profile", 32_000, 32_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := llmtest.New(truncatedTurn("cut"), truncatedTurn("longer cut"), llmtest.Turn{Text: "done"})
			a := mustNew(t, Config{
				Provider:          p,
				MaxTokens:         1_000,
				Profile:           &llm.Profile{ContextWindow: 200_000, MaxOutput: tc.maxOutput},
				EscalateMaxTokens: DefaultEscalatedMaxTokens,
			})

			events, final := collect(t, a.Run(context.Background(), userInput("go")))

			if final.Reason != ReasonCompleted || final.Text != "done" || p.Calls() != 3 {
				t.Fatalf("final = %+v after %d calls, want completed after 3", final, p.Calls())
			}
			retry := p.Requests[1]
			if retry.MaxTokens != tc.want {
				t.Fatalf("retry MaxTokens = %d, want %d", retry.MaxTokens, tc.want)
			}
			if len(retry.Messages) != len(p.Requests[0].Messages) {
				t.Fatal("the escalated retry added messages; it must repeat the same turn")
			}
			resume := p.Requests[2]
			msgs := resume.Messages
			if lastText(resume) != defaultResumePrompt || msgs[len(msgs)-2].Text() != "longer cut" {
				t.Fatalf("resume request ends with %+v, want truncated reply then resume prompt", msgs[len(msgs)-2:])
			}
			if !slices.ContainsFunc(events, func(ev Event) bool { return ev.Kind == KindRetry }) {
				t.Fatal("no KindRetry event for the discarded truncated reply")
			}
		})
	}
}

func TestMaxTokensResumeGivesUp(t *testing.T) {
	p := llmtest.New(truncatedTurn("1"), truncatedTurn("2"), truncatedTurn("3"), truncatedTurn("4"), truncatedTurn("never"))
	a := mustNew(t, Config{Provider: p})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted || final.Text != "4" || p.Calls() != 4 {
		t.Fatalf("final = %+v after %d calls, want completed with the 4th reply", final, p.Calls())
	}
}

// completeOnly is a provider that fails any streaming call, so a run can
// only succeed through Complete.
type completeOnly struct{ resp *llm.Response }

func (completeOnly) Stream(context.Context, llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, errors.New("streaming not supported"))
	}
}

func (p completeOnly) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return p.resp, nil
}

func TestNonStreamingEmitsOnce(t *testing.T) {
	p := completeOnly{resp: &llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{
			{Type: llm.BlockThinking, Thinking: "let me think"},
			llm.TextBlock("hello "),
			llm.TextBlock("world"),
		}},
		StopReason: llm.StopEndTurn,
	}}
	a := mustNew(t, Config{Provider: p, NonStreaming: true})

	events, final := collect(t, a.Run(context.Background(), userInput("hi")))

	if final.Reason != ReasonCompleted || final.Text != "hello world" {
		t.Fatalf("final = %+v, want completed with the full text", final)
	}
	var deltas []Event
	for _, ev := range events {
		if ev.Kind == KindText || ev.Kind == KindThinking {
			deltas = append(deltas, ev)
		}
	}
	if len(deltas) != 2 ||
		deltas[0].Kind != KindThinking || deltas[0].Text != "let me think" ||
		deltas[1].Kind != KindText || deltas[1].Text != "hello world" {
		t.Fatalf("content events = %+v, want one thinking then one text", deltas)
	}
}

// stopHook answers every BeforeStop with prompts[i] for its i-th call, and
// with the last prompt once they run out.
type stopHook struct {
	prompts []string
	calls   *int
}

func (stopHook) Name() string { return "stop_hook" }

func (h stopHook) BeforeStop(context.Context, *state.State, *llm.Response) (string, error) {
	i := min(*h.calls, len(h.prompts)-1)
	*h.calls++
	return h.prompts[i], nil
}

func TestBeforeStopContinues(t *testing.T) {
	var calls int
	p := llmtest.New(llmtest.Turn{Text: "first answer"}, llmtest.Turn{Text: "verified"})
	a := mustNew(t, Config{Provider: p, Middleware: []Middleware{stopHook{prompts: []string{"verify the file", ""}, calls: &calls}}})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted || final.Err != nil || final.Turns != 2 || final.Text != "verified" {
		t.Fatalf("final = %+v, want completed after 2 turns", final)
	}
	if p.Calls() != 2 || calls != 2 {
		t.Fatalf("model calls = %d, BeforeStop calls = %d; want 2 and 2", p.Calls(), calls)
	}
	msgs := final.State.Messages()
	if len(msgs) != 4 || msgs[1].Text() != "first answer" || msgs[2].Role != llm.RoleUser || msgs[2].Text() != "verify the file" {
		t.Fatalf("state messages = %+v, want the prompt right after the first answer", msgs)
	}
	if got := lastText(p.Requests[1]); got != "verify the file" {
		t.Fatalf("second request ends with %q, want the BeforeStop prompt", got)
	}
}

func TestBeforeStopCapped(t *testing.T) {
	var calls int
	// one spare: it must stay unused
	turns := make([]llmtest.Turn, 1+MaxStopContinuations+1)
	for i := range turns {
		turns[i] = llmtest.Turn{Text: fmt.Sprintf("answer %d", i)}
	}
	p := llmtest.New(turns...)
	a := mustNew(t, Config{Provider: p, Middleware: []Middleware{stopHook{prompts: []string{"again"}, calls: &calls}}})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted || final.Err != nil {
		t.Fatalf("final = %+v, want completed", final)
	}
	if p.Calls() != 1+MaxStopContinuations || calls != MaxStopContinuations {
		t.Fatalf("model calls = %d, BeforeStop calls = %d; want %d and %d",
			p.Calls(), calls, 1+MaxStopContinuations, MaxStopContinuations)
	}
}

func TestBeforeStopNotOnSettlement(t *testing.T) {
	var calls int
	p := llmtest.New(toolTurn("t1"), llmtest.Turn{Text: "summary"})
	a := mustNew(t, Config{
		Provider:   p,
		Tools:      []tool.Tool{echoTool()},
		MaxTurns:   1,
		Settlement: &Settlement{Prompt: settlementPrompt},
		Middleware: []Middleware{stopHook{prompts: []string{"keep going"}, calls: &calls}},
	})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonMaxTurns || final.Text != "summary" || p.Calls() != 2 {
		t.Fatalf("final = %+v after %d model calls, want max_turns after 2", final, p.Calls())
	}
	if calls != 0 {
		t.Fatalf("BeforeStop called %d times during settlement, want 0", calls)
	}
}

// failingStop fails BeforeStop with err.
type failingStop struct{ err error }

func (failingStop) Name() string { return "failing_stop" }

func (h failingStop) BeforeStop(context.Context, *state.State, *llm.Response) (string, error) {
	return "", h.err
}

func TestBeforeStopErrorEndsRun(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want TerminalReason
	}{
		{errors.New("boom"), ReasonMiddlewareError},
		{fmt.Errorf("enough: %w", ErrStopRun), ReasonHookStopped},
	} {
		a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "answer"}), Middleware: []Middleware{failingStop{tc.err}}})

		_, final := collect(t, a.Run(context.Background(), userInput("go")))

		if final.Reason != tc.want || !errors.Is(final.Err, tc.err) {
			t.Fatalf("final = %+v, want %s wrapping %v", final, tc.want, tc.err)
		}
	}
}

// namedStop answers BeforeStop with prompt and logs its name.
type namedStop struct {
	name, prompt string
	log          *[]string
}

func (h namedStop) Name() string { return h.name }

func (h namedStop) BeforeStop(context.Context, *state.State, *llm.Response) (string, error) {
	*h.log = append(*h.log, h.name)
	return h.prompt, nil
}

func TestBeforeStopReverseOrderFirstPromptWins(t *testing.T) {
	var log []string
	turns := make([]llmtest.Turn, 1+MaxStopContinuations)
	for i := range turns {
		turns[i] = llmtest.Turn{Text: "answer"}
	}
	p := llmtest.New(turns...)
	a := mustNew(t, Config{Provider: p, Middleware: []Middleware{
		namedStop{name: "outer", prompt: "from outer", log: &log},
		namedStop{name: "inner", prompt: "from inner", log: &log},
		namedStop{name: "innermost", log: &log},
	}})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted || p.Calls() != 1+MaxStopContinuations {
		t.Fatalf("final = %+v after %d model calls, want completed after %d", final, p.Calls(), 1+MaxStopContinuations)
	}
	if got := lastText(p.Requests[1]); got != "from inner" {
		t.Fatalf("continuation prompt = %q, want the first non-empty one in reverse order", got)
	}
	if want := []string{"innermost", "inner"}; !slices.Equal(log[:2], want) {
		t.Fatalf("hook order = %v, want %v first", log, want)
	}
}

// toolTurns scripts n tool-calling turns of the given usage, then, when
// finish is set, a final text turn.
func toolTurns(n int, usage llm.Usage, finish bool) []llmtest.Turn {
	turns := make([]llmtest.Turn, 0, n+1)
	for i := range n {
		turn := toolTurn(fmt.Sprintf("t%d", i))
		turn.Usage = usage
		turns = append(turns, turn)
	}
	if finish {
		turns = append(turns, llmtest.Turn{Text: "done"})
	}
	return turns
}

func TestDefaultMaxTurns(t *testing.T) {
	p := llmtest.New(toolTurns(DefaultMaxTurns+50, llm.Usage{}, false)...)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{echoTool()}})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonMaxTurns || final.Turns != DefaultMaxTurns || p.Calls() != DefaultMaxTurns {
		t.Fatalf("final = %+v after %d calls, want max_turns at %d turns", final, p.Calls(), DefaultMaxTurns)
	}
	if got := a.Config().MaxTurns; got != DefaultMaxTurns {
		t.Fatalf("Config().MaxTurns = %d, want %d", got, DefaultMaxTurns)
	}
}

func TestNegativeMaxTurnsUnbounded(t *testing.T) {
	p := llmtest.New(toolTurns(150, llm.Usage{}, true)...)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{echoTool()}, MaxTurns: -1})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted || final.Turns != 151 {
		t.Fatalf("final = reason %s turns %d, want completed after 151", final.Reason, final.Turns)
	}
}

func TestMaxTotalTokens(t *testing.T) {
	// Cache tokens count too: 400 + 300 + 200 + 100 = 1000 per turn.
	usage := llm.Usage{Input: 400, Output: 300, CacheRead: 200, CacheWrite: 100}
	p := llmtest.New(toolTurns(10, usage, false)...)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{echoTool()}, MaxTotalTokens: 2500})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonTokenLimit || final.Err != nil || final.Turns != 3 || p.Calls() != 3 {
		t.Fatalf("final = %+v after %d calls, want token_limit after 3 turns", final, p.Calls())
	}
	// The last reply's tool call did not run: State ends on the assistant
	// message, and history repair answers the call on the next run.
	msgs := final.State.Messages()
	if last := msgs[len(msgs)-1]; last.Role != llm.RoleAssistant || len(last.ToolUses()) != 1 {
		t.Fatalf("last message = %+v, want the assistant reply with its unanswered tool call", last)
	}
}

func TestTokenLimitBeatsMaxTurnsOnSameTurn(t *testing.T) {
	p := llmtest.New(toolTurns(5, llm.Usage{Input: 1000}, false)...)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{echoTool()}, MaxTurns: 2, MaxTotalTokens: 2000})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonTokenLimit || final.Turns != 2 {
		t.Fatalf("final = reason %s turns %d, want token_limit at turn 2", final.Reason, final.Turns)
	}
}

func TestTokenLimitSkipsEscalation(t *testing.T) {
	cut := truncatedTurn("cut")
	cut.Usage = llm.Usage{Output: 1000}
	p := llmtest.New(cut, llmtest.Turn{Text: "never reached"})
	a := mustNew(t, Config{Provider: p, MaxTokens: 1_000, EscalateMaxTokens: DefaultEscalatedMaxTokens, MaxTotalTokens: 1000})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonTokenLimit || p.Calls() != 1 {
		t.Fatalf("final = %+v after %d calls, want token_limit after 1 (no escalated retry)", final, p.Calls())
	}
}

func TestTokenLimitOnBeforeStopContinuation(t *testing.T) {
	var calls int
	first := llmtest.Turn{Text: `{"answer":"x"}`, Usage: llm.Usage{Output: 1000}}
	p := llmtest.New(first, llmtest.Turn{Text: "never reached"})
	a := mustNew(t, Config{
		Provider: p, OutputSchema: answerSchema, MaxTotalTokens: 1000,
		Middleware: []Middleware{stopHook{prompts: []string{"verify"}, calls: &calls}},
	})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonTokenLimit || final.Output != nil || p.Calls() != 1 {
		t.Fatalf("final = %+v after %d calls, want token_limit, no output, 1 call", final, p.Calls())
	}
}

func TestSettlementPastTokenCapStillEndsMaxTurns(t *testing.T) {
	// Turn 1 stays under the cap and hits MaxTurns; the settlement call then
	// crosses the cap, which must not turn the reason into token_limit.
	t1 := toolTurn("t1")
	t1.Usage = llm.Usage{Input: 400}
	settle := llmtest.Turn{Text: "summary", Usage: llm.Usage{Input: 5000}}
	p := llmtest.New(t1, settle)
	a := mustNew(t, Config{
		Provider: p, Tools: []tool.Tool{echoTool()}, MaxTurns: 1, MaxTotalTokens: 1000,
		Settlement: &Settlement{Prompt: settlementPrompt},
	})

	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonMaxTurns || final.Text != "summary" || p.Calls() != 2 {
		t.Fatalf("final = %+v after %d calls, want max_turns with the settlement summary", final, p.Calls())
	}
}

func TestCustomBudgetNudge(t *testing.T) {
	p := llmtest.New(textTurn("a", 100), textTurn("b", 100))
	a := mustNew(t, Config{Provider: p, TokenBudget: 10_000, BudgetNudge: "go on, %d left"})
	collect(t, a.Run(context.Background(), userInput("go")))
	if got := lastText(p.Requests[1]); got != "go on, 9900 left" {
		t.Fatalf("nudge = %q, want custom text", got)
	}
}

func TestBudgetNudgeWithoutVerb(t *testing.T) {
	p := llmtest.New(textTurn("a", 100), textTurn("b", 100))
	a := mustNew(t, Config{Provider: p, TokenBudget: 10_000, BudgetNudge: "keep going"})
	collect(t, a.Run(context.Background(), userInput("go")))
	if got := lastText(p.Requests[1]); got != "keep going" {
		t.Fatalf("nudge = %q, want %q", got, "keep going")
	}
}

func TestCustomResumePrompt(t *testing.T) {
	p := llmtest.New(truncatedTurn("cut"), llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: p, ResumePrompt: "continue"})
	collect(t, a.Run(context.Background(), userInput("go")))
	if got := lastText(p.Requests[1]); got != "continue" {
		t.Fatalf("resume prompt = %q, want %q", got, "continue")
	}
}

func TestEscalateMaxTokensInt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		escalate  int
		wantCalls int
		want      int
	}{
		{"custom limit", 30_000, 3, 30_000},
		{"capped at the profile", 100_000, 3, 64_000},
		{"off", 0, 3, 1_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := llmtest.New(truncatedTurn("cut"), truncatedTurn("longer cut"), llmtest.Turn{Text: "done"})
			a := mustNew(t, Config{
				Provider:          p,
				MaxTokens:         1_000,
				Profile:           &llm.Profile{ContextWindow: 200_000, MaxOutput: 64_000},
				EscalateMaxTokens: tc.escalate,
			})
			collect(t, a.Run(context.Background(), userInput("go")))
			if p.Calls() != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", p.Calls(), tc.wantCalls)
			}
			if got := p.Requests[1].MaxTokens; got != tc.want {
				t.Fatalf("second request MaxTokens = %d, want %d", got, tc.want)
			}
		})
	}
}
