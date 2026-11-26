package llmtest_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func TestScriptedStreamsTextThenTools(t *testing.T) {
	s := llmtest.New(llmtest.Turn{
		Thinking: "hmm",
		Text:     "let me look",
		ToolCalls: []llmtest.Call{
			{ID: "t1", Name: "read", Input: map[string]string{"path": "a"}},
			{ID: "t2", Name: "other", Input: map[string]int{"x": 1}},
		},
		Usage: llm.Usage{Input: 10, Output: 5},
	})

	resp, err := llm.Collect(t.Context(), s, llm.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if resp.Usage != (llm.Usage{Input: 10, Output: 5}) {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	c := resp.Message.Content
	if len(c) != 4 || c[0].Thinking != "hmm" || c[1].Text != "let me look" {
		t.Fatalf("content = %+v", c)
	}
	uses := resp.Message.ToolUses()
	if len(uses) != 2 || uses[0].ID != "t1" || string(uses[0].Input) != `{"path":"a"}` {
		t.Errorf("tool uses = %+v", uses)
	}
	if string(uses[1].Input) != `{"x":1}` {
		t.Errorf("second input = %s", uses[1].Input)
	}
	if len(s.Requests) != 1 || s.Calls() != 1 {
		t.Errorf("requests = %d, calls = %d, want 1", len(s.Requests), s.Calls())
	}
}

func TestScriptedDefaultStopReasonIsEndTurn(t *testing.T) {
	s := llmtest.New(llmtest.Turn{Text: "done"})
	resp, err := llm.Collect(t.Context(), s, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != llm.StopEndTurn {
		t.Errorf("StopReason = %q, want end_turn", resp.StopReason)
	}
}

func TestScriptedExhausted(t *testing.T) {
	s := llmtest.New()
	_, err := s.Complete(t.Context(), llm.Request{})
	if err == nil || err.Error() != "llmtest: script exhausted" {
		t.Errorf("err = %v", err)
	}
}

func TestScriptedTurnErrReturnedBeforeEvents(t *testing.T) {
	boom := errors.New("boom")
	s := llmtest.New(llmtest.Turn{Text: "never", Err: boom})
	for ev, err := range s.Stream(t.Context(), llm.Request{}) {
		if !errors.Is(err, boom) {
			t.Fatalf("first yield = %+v, %v", ev, err)
		}
		return
	}
	t.Fatal("stream yielded nothing")
}

func TestScriptedCompleteMatchesStream(t *testing.T) {
	turn := llmtest.Turn{
		Text:      "hi",
		ToolCalls: []llmtest.Call{{ID: "t1", Name: "x", Input: map[string]int{"n": 1}}},
	}
	streamed, err := llm.Collect(t.Context(), llmtest.New(turn), llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := llmtest.New(turn).Complete(t.Context(), llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(streamed, completed) {
		t.Errorf("Complete = %+v, Collect = %+v", completed, streamed)
	}
}

func TestScriptedRecordsIndependentRequestCopies(t *testing.T) {
	s := llmtest.New(llmtest.Turn{Text: "ok"})
	req := llm.Request{
		System:   []string{"sys"},
		Messages: []llm.Message{llm.UserText("hello")},
	}
	if _, err := s.Complete(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	req.System[0] = "changed"
	req.Messages[0].Content[0].Text = "changed"

	got := s.Requests[0]
	if got.System[0] != "sys" || got.Messages[0].Text() != "hello" {
		t.Errorf("recorded request was mutated: %+v", got)
	}
}

func TestScriptedRawToolInputIsVerbatim(t *testing.T) {
	s := llmtest.New(llmtest.Turn{
		RawToolInput: true,
		ToolCalls:    []llmtest.Call{{ID: "t1", Name: "x", Input: `{"broken":`}},
	})
	resp, err := s.Complete(t.Context(), llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Message.ToolUses()[0].Input); got != `{"broken":` {
		t.Errorf("input = %q", got)
	}
}

func TestErrExhaustedExported(t *testing.T) {
	_, err := llmtest.New().Complete(t.Context(), llm.Request{})
	if !errors.Is(err, llmtest.ErrExhausted) {
		t.Errorf("err = %v, want ErrExhausted", err)
	}
}

func TestFuncTurnSeesRequest(t *testing.T) {
	s := llmtest.New(llmtest.Func(func(r llm.Request) llmtest.Turn {
		return llmtest.Turn{Text: r.Messages[0].Text()}
	}))
	resp, err := s.Complete(t.Context(), llm.Request{Messages: []llm.Message{llm.UserText("echo")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Message.Text(); got != "echo" {
		t.Errorf("text = %q, want echo", got)
	}
}

func TestFuncTurnNestedFuncIsError(t *testing.T) {
	inner := llmtest.Func(func(llm.Request) llmtest.Turn { return llmtest.Turn{Text: "x"} })
	s := llmtest.New(llmtest.Func(func(llm.Request) llmtest.Turn { return inner }))
	_, err := s.Complete(t.Context(), llm.Request{})
	if err == nil || err.Error() != "llmtest: Func returned a Func turn" {
		t.Errorf("err = %v", err)
	}
}

func TestFuncTurnErrIsReturned(t *testing.T) {
	boom := errors.New("boom")
	s := llmtest.New(llmtest.Func(func(llm.Request) llmtest.Turn { return llmtest.Turn{Err: boom} }))
	if _, err := s.Complete(t.Context(), llm.Request{}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestTurnModelAndID(t *testing.T) {
	s := llmtest.New(llmtest.Turn{Text: "x", Model: "m1", ID: "r1"})
	resp, err := s.Complete(t.Context(), llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "m1" || resp.ID != "r1" {
		t.Errorf("Model = %q, ID = %q", resp.Model, resp.ID)
	}
}

func TestScriptedCancelledCtxDoesNotConsumeTurn(t *testing.T) {
	s := llmtest.New(llmtest.Turn{Text: "ok"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Complete(ctx, llm.Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if s.Calls() != 0 {
		t.Errorf("Calls = %d, want 0", s.Calls())
	}
	if _, err := s.Complete(t.Context(), llm.Request{}); err != nil {
		t.Errorf("turn was consumed by the cancelled call: %v", err)
	}
}
