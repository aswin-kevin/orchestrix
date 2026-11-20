package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

func TestInterruptIsError(t *testing.T) {
	var err error = &Interrupt{Requests: []ActionRequest{{ToolUseID: "t1"}}}
	wrapped := fmt.Errorf("run: %w", err)

	var got *Interrupt
	if !errors.As(wrapped, &got) || len(got.Requests) != 1 {
		t.Fatalf("errors.As failed: %v", wrapped)
	}
	if want := "agent: interrupted: 1 action(s) awaiting a decision"; got.Error() != want {
		t.Fatalf("Error() = %q, want %q", got.Error(), want)
	}
}

// callLog records which tool calls ran, by tool_use id, and the resume value
// each invocation received.
type callLog struct {
	mu      sync.Mutex
	runs    []string
	resumes map[string][]*tool.ResumeValue
}

func newCallLog() *callLog {
	return &callLog{resumes: map[string][]*tool.ResumeValue{}}
}

func (l *callLog) add(c *tool.Call) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs = append(l.runs, c.ToolUseID)
	l.resumes[c.ToolUseID] = append(l.resumes[c.ToolUseID], c.Resume)
}

// ran returns the ids of the calls that ran, in the order they ran.
func (l *callLog) ran() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.runs)
}

// count returns how many times the call with the given id ran.
func (l *callLog) count(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.resumes[id])
}

// readTool is read-only, so the default permission mode allows it.
func readTool(log *callLog) tool.Tool {
	return tool.New("read", "Read.",
		func(_ context.Context, in echoInput, c *tool.Call) (tool.Result, error) {
			log.add(c)
			return tool.Text("read " + in.Text), nil
		},
		tool.WithReadOnly())
}

// writeTool is not read-only, so without rules the default mode asks for it.
func writeTool(log *callLog) tool.Tool {
	return tool.New("write_file", "Write.",
		func(_ context.Context, in writeInput, c *tool.Call) (tool.Result, error) {
			log.add(c)
			return tool.Text("wrote " + in.Text), nil
		})
}

func readCall(id string) llmtest.Call {
	return llmtest.Call{ID: id, Name: "read", Input: map[string]any{"text": id}}
}

func writeCall(id string) llmtest.Call {
	return llmtest.Call{ID: id, Name: "write_file", Input: map[string]any{"text": id}}
}

// interruptedRun runs an agent whose first turn makes calls and whose second
// turn answers "done", and requires the run to stop interrupted. The agent is
// returned so the test can resume it.
func interruptedRun(t *testing.T, cfg Config, calls ...llmtest.Call) (*Agent, []Event, *Final) {
	t.Helper()
	cfg.Provider = llmtest.New(llmtest.Turn{ToolCalls: calls}, llmtest.Turn{Text: "done"})
	a := mustNew(t, cfg)
	events, final := collect(t, a.Run(context.Background(), userInput("go")))
	if final.Reason != ReasonInterrupted || final.Err != nil {
		t.Fatalf("final = %+v, want interrupted without error", final)
	}
	return a, events, final
}

// interruptEvent returns the single KindInterrupt event, which must come
// right before KindFinal.
func interruptEvent(t *testing.T, events []Event) *Interrupt {
	t.Helper()
	var found []*Interrupt
	for _, ev := range events {
		if ev.Kind == KindInterrupt {
			found = append(found, ev.Interrupt)
		}
	}
	if len(found) != 1 || found[0] == nil {
		t.Fatalf("got %d KindInterrupt events, want exactly one with an Interrupt", len(found))
	}
	if events[len(events)-2].Kind != KindInterrupt {
		t.Fatalf("event before KindFinal = %s, want KindInterrupt", events[len(events)-2].Kind)
	}
	return found[0]
}

// requestIDs lists the tool_use ids of the interrupt's requests.
func requestIDs(i *Interrupt) []string {
	var ids []string
	for _, r := range i.Requests {
		ids = append(ids, r.ToolUseID)
	}
	return ids
}

// resumeCompleted resumes s and requires the run to complete. It returns the
// tool_result blocks answering the interrupted turn.
func resumeCompleted(t *testing.T, a *Agent, s *state.State, decisions ...Decision) []llm.Block {
	t.Helper()
	_, final := collect(t, a.Resume(context.Background(), "", s, decisions))
	if final.Reason != ReasonCompleted || final.Err != nil || final.Text != "done" {
		t.Fatalf("resumed final = %+v, want completed with text", final)
	}
	if _, ok := pendingKey.Get(final.State); ok {
		t.Fatal("pending record still in State after the turn completed")
	}
	return final.State.Messages()[2].Content
}

func TestAskWithoutApproverInterrupts(t *testing.T) {
	log := newCallLog()
	_, events, final := interruptedRun(t, Config{Tools: []tool.Tool{readTool(log), writeTool(log)}},
		readCall("r1"), writeCall("w"), readCall("r2"))

	if ran := log.ran(); !slices.Equal(ran, []string{"r1"}) {
		t.Fatalf("calls run = %v, want only r1", ran)
	}
	got := interruptEvent(t, events)
	if !slices.Equal(requestIDs(got), []string{"w"}) {
		t.Fatalf("interrupt requests = %+v, want w only", got.Requests)
	}
	req := got.Requests[0]
	if req.Tool != "write_file" || string(req.Input) != `{"text":"w"}` || req.Reason != "approve write_file?" {
		t.Fatalf("request = %+v", req)
	}
	if want := []DecisionType{Approve, Edit, Reject, Respond}; !slices.Equal(req.Allowed, want) {
		t.Fatalf("allowed = %v, want %v", req.Allowed, want)
	}
	msgs := final.State.Messages()
	if len(msgs) != 2 || msgs[1].Role != llm.RoleAssistant {
		t.Fatalf("state messages = %+v, want user + assistant with no tool results yet", msgs)
	}
	if _, ok := pendingKey.Get(final.State); !ok {
		t.Fatal("no pending record in State")
	}
}

func TestAllAsksCollectedInOneInterrupt(t *testing.T) {
	log := newCallLog()
	_, events, _ := interruptedRun(t, Config{Tools: []tool.Tool{readTool(log), writeTool(log)}},
		writeCall("w1"), readCall("r1"), writeCall("w2"))

	if ran := log.ran(); len(ran) != 0 {
		t.Fatalf("calls run = %v, want none: nothing starts after the first pending call", ran)
	}
	if ids := requestIDs(interruptEvent(t, events)); !slices.Equal(ids, []string{"w1", "w2"}) {
		t.Fatalf("interrupt requests = %v, want [w1 w2]", ids)
	}
}

func TestResumeApproveRunsPendingThenRest(t *testing.T) {
	log := newCallLog()
	a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{readTool(log), writeTool(log)}},
		readCall("r1"), writeCall("w"), readCall("r2"))

	results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "w", Type: Approve})

	if ran := log.ran(); !slices.Equal(ran, []string{"r1", "w", "r2"}) {
		t.Fatalf("calls run = %v, want r1, w, r2 once each", ran)
	}
	ids := []string{"r1", "w", "r2"}
	want := []string{"read r1", "wrote w", "read r2"}
	if len(results) != len(want) {
		t.Fatalf("tool results = %+v, want %d", results, len(want))
	}
	for i := range want {
		if r := results[i]; r.ToolUseID != ids[i] || r.IsError || resultText(r) != want[i] {
			t.Fatalf("result %d = %+v, want %q for %s", i, r, want[i], ids[i])
		}
	}
}

func TestResumeEditValidates(t *testing.T) {
	t.Run("invalid input", func(t *testing.T) {
		log := newCallLog()
		a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log)}}, writeCall("w"))

		results := resumeCompleted(t, a, final.State,
			Decision{ToolUseID: "w", Type: Edit, Input: json.RawMessage(`{"text":5}`)})

		want := "Edited input is invalid: text: expected string, got number"
		if r := results[0]; !r.IsError || resultText(r) != want {
			t.Fatalf("result = %+v, want %q", r, want)
		}
		if ran := log.ran(); len(ran) != 0 {
			t.Fatal("tool ran with invalid edited input")
		}
	})

	t.Run("valid input", func(t *testing.T) {
		log := newCallLog()
		a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log)}}, writeCall("w"))

		results := resumeCompleted(t, a, final.State,
			Decision{ToolUseID: "w", Type: Edit, Input: json.RawMessage(`{"text":"edited"}`)})

		if r := results[0]; r.IsError || resultText(r) != "wrote edited" {
			t.Fatalf("result = %+v, want the tool run with the edited input", r)
		}
	})
}

func TestResumeReject(t *testing.T) {
	cases := []struct{ message, want string }{
		{"too risky", "User rejected this action: too risky"},
		{"", "User rejected this action"},
	}
	for _, c := range cases {
		log := newCallLog()
		a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log)}}, writeCall("w"))

		results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "w", Type: Reject, Message: c.message})

		if r := results[0]; !r.IsError || resultText(r) != c.want {
			t.Fatalf("result = %+v, want %q", r, c.want)
		}
		if ran := log.ran(); len(ran) != 0 {
			t.Fatal("rejected tool ran")
		}
	}
}

func TestResumeRespondToPermissionAsk(t *testing.T) {
	log := newCallLog()
	a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log)}}, writeCall("w"))

	results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "w", Type: Respond, Message: "already written"})

	if r := results[0]; r.IsError || resultText(r) != "already written" {
		t.Fatalf("result = %+v, want the response as a successful result", r)
	}
	if ran := log.ran(); len(ran) != 0 {
		t.Fatal("tool ran although the human answered for it")
	}
}

// askUserTool pauses the run on its first invocation and answers with the
// human's response once resumed.
func askUserTool(log *callLog) tool.Tool {
	return tool.New("ask_user", "Ask the user.",
		func(_ context.Context, _ echoInput, c *tool.Call) (tool.Result, error) {
			log.add(c)
			if c.Resume == nil {
				return tool.Result{}, &Interrupt{}
			}
			return tool.Text("answer: " + c.Resume.Message), nil
		},
		tool.WithReadOnly())
}

func TestToolOriginInterruptRespond(t *testing.T) {
	log := newCallLog()
	a, events, final := interruptedRun(t, Config{Tools: []tool.Tool{askUserTool(log)}},
		llmtest.Call{ID: "q", Name: "ask_user", Input: map[string]any{"text": "colour?"}})

	req := interruptEvent(t, events).Requests[0]
	if req.ToolUseID != "q" || req.Tool != "ask_user" {
		t.Fatalf("request = %+v", req)
	}
	if want := []DecisionType{Approve, Reject, Respond}; !slices.Equal(req.Allowed, want) {
		t.Fatalf("allowed = %v, want the default %v", req.Allowed, want)
	}

	results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "q", Type: Respond, Message: "blue"})

	if r := results[0]; r.IsError || resultText(r) != "answer: blue" {
		t.Fatalf("result = %+v, want the tool's answer", r)
	}
	resumes := log.resumes["q"]
	if len(resumes) != 2 || resumes[0] != nil || resumes[1] == nil ||
		resumes[1].Type != tool.ResumeRespond || resumes[1].Message != "blue" {
		t.Fatalf("resume values seen by the tool = %+v, want nil then respond/blue", resumes)
	}
}

// rejectAwareTool raises an interrupt, flagged ReinvokeOnReject when flag is
// set, and answers a resumed call with the decision it was re-invoked with.
func rejectAwareTool(log *callLog, flag bool) tool.Tool {
	return tool.New("ask_user", "Ask the user.",
		func(_ context.Context, _ echoInput, c *tool.Call) (tool.Result, error) {
			log.add(c)
			if c.Resume == nil {
				return tool.Result{}, &Interrupt{ReinvokeOnReject: flag}
			}
			return tool.Text("tool saw " + c.Resume.Type + ": " + c.Resume.Message), nil
		},
		tool.WithReadOnly())
}

func TestToolOriginInterruptReinvokeOnReject(t *testing.T) {
	t.Run("flagged tool handles the rejection", func(t *testing.T) {
		log := newCallLog()
		a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{rejectAwareTool(log, true)}},
			llmtest.Call{ID: "q", Name: "ask_user", Input: map[string]any{"text": "colour?"}})
		// The flag is part of the pending record, so it survives a checkpoint.
		data, err := json.Marshal(final.State)
		if err != nil {
			t.Fatal(err)
		}
		restored := state.New()
		if err := json.Unmarshal(data, restored); err != nil {
			t.Fatal(err)
		}

		results := resumeCompleted(t, a, restored, Decision{ToolUseID: "q", Type: Reject, Message: "no"})

		if r := results[0]; resultText(r) != "tool saw reject: no" {
			t.Fatalf("result = %+v, want the tool's own answer to the rejection", r)
		}
		resumes := log.resumes["q"]
		if len(resumes) != 2 || resumes[1] == nil || resumes[1].Type != tool.ResumeReject || resumes[1].Message != "no" {
			t.Fatalf("resume values seen by the tool = %+v, want nil then reject/no", resumes)
		}
	})
	t.Run("unflagged tool is answered without running", func(t *testing.T) {
		log := newCallLog()
		a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{rejectAwareTool(log, false)}},
			llmtest.Call{ID: "q", Name: "ask_user", Input: map[string]any{"text": "colour?"}})

		results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "q", Type: Reject, Message: "no"})

		if r := results[0]; !r.IsError || resultText(r) != "User rejected this action: no" {
			t.Fatalf("result = %+v, want the agent's rejection", r)
		}
		if n := len(log.resumes["q"]); n != 1 {
			t.Fatalf("tool ran %d times, want once (not re-invoked on reject)", n)
		}
	})
}

// confirmWrapper is a ToolWrapper that pauses every call until it is resumed.
type confirmWrapper struct{}

func (confirmWrapper) Name() string { return "confirm" }

func (confirmWrapper) WrapTool(ctx context.Context, call *ToolCall, next ToolHandler) (tool.Result, error) {
	if call.Call.Resume == nil {
		return tool.Result{}, fmt.Errorf("confirm: %w", &Interrupt{Requests: []ActionRequest{{
			Reason:  "confirm first",
			Allowed: []DecisionType{Approve, Reject},
		}}})
	}
	return next(ctx, call)
}

func TestToolWrapperInterrupt(t *testing.T) {
	log := newCallLog()
	cfg := Config{Tools: []tool.Tool{readTool(log)}, Middleware: []Middleware{confirmWrapper{}}}
	a, events, final := interruptedRun(t, cfg, readCall("r"))

	req := interruptEvent(t, events).Requests[0]
	if req.Reason != "confirm first" || !slices.Equal(req.Allowed, []DecisionType{Approve, Reject}) {
		t.Fatalf("request = %+v, want the wrapper's reason and allowed list", req)
	}

	_, invalid := collect(t, a.Resume(context.Background(), "", final.State,
		[]Decision{{ToolUseID: "r", Type: Respond}}))
	if invalid.Reason != ReasonInvalidResume || !errors.Is(invalid.Err, ErrInvalidDecision) {
		t.Fatalf("final = %+v, want invalid_resume with ErrInvalidDecision", invalid)
	}

	results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "r", Type: Approve})
	if r := results[0]; r.IsError || resultText(r) != "read r" {
		t.Fatalf("result = %+v", r)
	}
	if n := log.count("r"); n != 1 {
		t.Fatalf("tool ran %d times, want once (the wrapper stopped the first attempt)", n)
	}
}

func TestResumeMissingDecision(t *testing.T) {
	log := newCallLog()
	a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log)}}, writeCall("w1"), writeCall("w2"))
	before, err := json.Marshal(final.State)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		state     *state.State
		decisions []Decision
		want      error // nil: any error will do
	}{
		{"missing decision", final.State, []Decision{{ToolUseID: "w1", Type: Approve}}, ErrMissingDecision},
		{"no pending interrupt", state.New(), nil, ErrNoPendingInterrupt},
		{"unknown tool call", final.State, []Decision{
			{ToolUseID: "w1", Type: Approve}, {ToolUseID: "w2", Type: Approve}, {ToolUseID: "nope", Type: Approve},
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events, got := collect(t, a.Resume(context.Background(), "", c.state, c.decisions))
			if len(events) != 1 || got.Reason != ReasonInvalidResume || got.Err == nil || got.State != c.state {
				t.Fatalf("events = %+v, final = %+v; want a single invalid_resume final carrying the State", events, got)
			}
			if c.want != nil && !errors.Is(got.Err, c.want) {
				t.Fatalf("err = %v, want %v", got.Err, c.want)
			}
		})
	}

	after, err := json.Marshal(final.State)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || len(log.ran()) != 0 {
		t.Fatal("a rejected Resume changed the State or ran a tool")
	}
}

func TestPendingSurvivesJSON(t *testing.T) {
	log := newCallLog()
	a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{readTool(log), writeTool(log)}},
		readCall("r1"), writeCall("w"), readCall("r2"))

	data, err := json.Marshal(final.State)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	restored := state.New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	results := resumeCompleted(t, a, restored, Decision{ToolUseID: "w", Type: Approve})

	if ran := log.ran(); !slices.Equal(ran, []string{"r1", "w", "r2"}) {
		t.Fatalf("calls run = %v, want r1, w, r2 once each", ran)
	}
	if r := results[0]; r.ToolUseID != "r1" || resultText(r) != "read r1" {
		t.Fatalf("first result = %+v, want r1's recorded result", r)
	}
}

func TestParallelBatchKeepsRunningCalls(t *testing.T) {
	log := newCallLog()
	started := make(chan struct{})
	// slow pauses the run, but only once fast is running, so both calls of
	// the batch are deterministically in flight together.
	slow := tool.New("slow", "Interrupt once the neighbour runs.",
		func(_ context.Context, _ echoInput, c *tool.Call) (tool.Result, error) {
			log.add(c)
			if c.Resume == nil {
				<-started
				return tool.Result{}, &Interrupt{}
			}
			return tool.Text("slow done"), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())
	fast := tool.New("fast", "Run alongside.",
		func(_ context.Context, _ echoInput, c *tool.Call) (tool.Result, error) {
			log.add(c)
			close(started)
			return tool.Text("fast done"), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())

	a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{slow, fast}},
		llmtest.Call{ID: "s", Name: "slow", Input: map[string]any{"text": "s"}},
		llmtest.Call{ID: "f", Name: "fast", Input: map[string]any{"text": "f"}})

	results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "s", Type: Approve})

	if log.count("f") != 1 || log.count("s") != 2 {
		t.Fatalf("runs: fast %d, slow %d; want fast once (result kept) and slow twice", log.count("f"), log.count("s"))
	}
	if resultText(results[0]) != "slow done" || resultText(results[1]) != "fast done" {
		t.Fatalf("results = %+v", results)
	}
}

func TestRunClearsPendingInterrupt(t *testing.T) {
	log := newCallLog()
	a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log)}}, writeCall("w"))

	_, next := collect(t, a.Run(context.Background(), Input{
		State:    final.State,
		Messages: []llm.Message{llm.UserText("never mind")},
	}))

	if next.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", next)
	}
	if _, ok := pendingKey.Get(next.State); ok {
		t.Fatal("Run kept the pending record")
	}
	_, resumed := collect(t, a.Resume(context.Background(), "", next.State, nil))
	if !errors.Is(resumed.Err, ErrNoPendingInterrupt) {
		t.Fatalf("Resume after Run: err = %v, want ErrNoPendingInterrupt", resumed.Err)
	}
}

func TestDecidedCallsRunBeforeNeverStartedOnes(t *testing.T) {
	log := newCallLog()
	a, events, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log), askUserTool(log)}},
		writeCall("w1"), llmtest.Call{ID: "x", Name: "ask_user", Input: map[string]any{"text": "x"}}, writeCall("w2"))
	if ids := requestIDs(interruptEvent(t, events)); !slices.Equal(ids, []string{"w1", "w2"}) {
		t.Fatalf("first interrupt = %v, want [w1 w2]", ids)
	}

	events, again := collect(t, a.Resume(context.Background(), "", final.State, []Decision{
		{ToolUseID: "w1", Type: Approve},
		{ToolUseID: "w2", Type: Edit, Input: json.RawMessage(`{"text":"edited"}`)},
	}))

	if again.Reason != ReasonInterrupted {
		t.Fatalf("final = %+v, want interrupted again by x", again)
	}
	if ids := requestIDs(interruptEvent(t, events)); !slices.Equal(ids, []string{"x"}) {
		t.Fatalf("second interrupt = %v, want [x]", ids)
	}
	if ran := log.ran(); !slices.Equal(ran, []string{"w1", "w2", "x"}) {
		t.Fatalf("calls run = %v, want the decided w1 and w2, then x", ran)
	}
	if in := toolUseInput(events, "w2"); in != `{"text":"edited"}` {
		t.Fatalf("KindToolUse input for w2 = %s, want the edited input", in)
	}

	results := resumeCompleted(t, a, again.State, Decision{ToolUseID: "x", Type: Respond, Message: "ok"})

	want := []string{"wrote w1", "answer: ok", "wrote edited"}
	for i := range want {
		if got := resultText(results[i]); got != want[i] {
			t.Fatalf("result %d = %q, want %q", i, got, want[i])
		}
	}
	if log.count("w1") != 1 || log.count("w2") != 1 {
		t.Fatalf("runs: w1 %d, w2 %d; want each exactly once", log.count("w1"), log.count("w2"))
	}
}

// toolUseInput returns the input of the KindToolUse event for id.
func toolUseInput(events []Event, id string) string {
	for _, ev := range events {
		if ev.Kind == KindToolUse && ev.ToolID == id {
			return string(ev.Input)
		}
	}
	return ""
}

func TestNoCallStartsAfterHaltWhileWaitingForSlot(t *testing.T) {
	log := newCallLog()
	pause := tool.New("pause", "Interrupt until resumed.",
		func(_ context.Context, _ echoInput, c *tool.Call) (tool.Result, error) {
			log.add(c)
			if c.Resume == nil {
				return tool.Result{}, &Interrupt{}
			}
			return tool.Text("paused"), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())
	next := tool.New("next", "Run after pause.",
		func(_ context.Context, _ echoInput, c *tool.Call) (tool.Result, error) {
			log.add(c)
			return tool.Text("next done"), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())

	// With one slot, n waits for p's slot; p interrupts before releasing it.
	a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{pause, next}, MaxConcurrency: 1},
		llmtest.Call{ID: "p", Name: "pause", Input: map[string]any{"text": "p"}},
		llmtest.Call{ID: "n", Name: "next", Input: map[string]any{"text": "n"}})

	if n := log.count("n"); n != 0 {
		t.Fatalf("n ran %d times before the resume, want 0: it got its slot after p interrupted", n)
	}
	results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "p", Type: Approve})
	if log.count("n") != 1 || resultText(results[1]) != "next done" {
		t.Fatalf("n ran %d times with result %+v, want once after the resume", log.count("n"), results[1])
	}
}

// noteOnResume is a BeforeAgent hook that appends a message once on is set.
type noteOnResume struct{ on *atomic.Bool }

func (noteOnResume) Name() string { return "note_on_resume" }

func (h noteOnResume) BeforeAgent(_ context.Context, s *state.State) error {
	if h.on.Load() {
		s.AppendMessages(llm.UserText("hook note"))
	}
	return nil
}

func TestResumeInsertsResultsBeforeHookMessages(t *testing.T) {
	log := newCallLog()
	var on atomic.Bool
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w")}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{writeTool(log)}, Middleware: []Middleware{noteOnResume{&on}}})
	_, final := collect(t, a.Run(context.Background(), userInput("go")))
	if final.Reason != ReasonInterrupted {
		t.Fatalf("final = %+v, want interrupted", final)
	}

	on.Store(true)
	resumeCompleted(t, a, final.State, Decision{ToolUseID: "w", Type: Approve})

	msgs := final.State.Messages()
	if len(msgs) != 5 || msgs[2].Content[0].ToolUseID != "w" || msgs[3].Text() != "hook note" {
		t.Fatalf("history = %+v, want user, assistant, tool results, hook note, assistant", msgs)
	}
	sent := p.Requests[1].Messages
	if r := sent[2].Content[0]; r.Type != llm.BlockToolResult || r.ToolUseID != "w" || r.IsError || resultText(r) != "wrote w" {
		t.Fatalf("message after the tool_use in the request = %+v, want w's real result", sent[2])
	}
}

func TestHeldCallPanickingPermissionCheckIsNotStarted(t *testing.T) {
	log := newCallLog()
	boom := tool.New("boom", "Panic in the permission check.",
		func(context.Context, struct{}, *tool.Call) (tool.Result, error) { return tool.Text("ok"), nil },
		tool.WithPermission(func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			panic("bad check")
		}))
	a, events, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log), boom}},
		writeCall("w"), llmtest.Call{ID: "b", Name: "boom"})

	if ids := requestIDs(interruptEvent(t, events)); !slices.Equal(ids, []string{"w"}) {
		t.Fatalf("interrupt = %v, want [w]", ids)
	}
	results := resumeCompleted(t, a, final.State, Decision{ToolUseID: "w", Type: Approve})
	if r := results[1]; !r.IsError || resultText(r) != "tool boom panicked: bad check" {
		t.Fatalf("boom result = %+v, want the panic result once it is scheduled", r)
	}
}

func TestPendingRecordUsesSnakeCaseFields(t *testing.T) {
	log := newCallLog()
	_, _, final := interruptedRun(t, Config{Tools: []tool.Tool{readTool(log), writeTool(log)}},
		readCall("r"), writeCall("w"))

	data, err := json.Marshal(final.State)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tool_use_id":"w"`, `"tool":"write_file"`, `"allowed":["approve"`, `"reason":"approve write_file?"`, `"content":[`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("serialized state lacks %s: %s", want, data)
		}
	}
}

func TestPublicTypesJSONFieldNames(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{Decision{ToolUseID: "t", Type: Edit, Input: json.RawMessage(`{}`), Message: "m"},
			`{"tool_use_id":"t","type":"edit","input":{},"message":"m"}`},
		{Interrupt{Requests: []ActionRequest{{ToolUseID: "t", Tool: "x", Input: json.RawMessage(`{}`), Reason: "r", Allowed: []DecisionType{Approve}}}},
			`{"requests":[{"tool_use_id":"t","tool":"x","input":{},"reason":"r","allowed":["approve"]}]}`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.value)
		if err != nil || string(got) != c.want {
			t.Errorf("json.Marshal(%T) = %s, %v; want %s", c.value, got, err, c.want)
		}
	}
}

// askTwice makes a turn whose first call interrupts and whose second call,
// held back by the first, interrupts once it runs on resume.
func askTwice() []llmtest.Call {
	return []llmtest.Call{
		{ID: "q1", Name: "ask_user", Input: map[string]any{"text": "one"}},
		{ID: "q2", Name: "ask_user", Input: map[string]any{"text": "two"}},
	}
}

func TestResumeAgainAfterHookAppendedAndCallReinterrupted(t *testing.T) {
	log := newCallLog()
	var on atomic.Bool
	p := llmtest.New(llmtest.Turn{ToolCalls: askTwice()}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{askUserTool(log)}, Middleware: []Middleware{noteOnResume{&on}}})
	_, final := collect(t, a.Run(context.Background(), userInput("go")))
	if final.Reason != ReasonInterrupted {
		t.Fatalf("final = %+v, want interrupted", final)
	}

	on.Store(true)
	_, second := collect(t, a.Resume(context.Background(), "", final.State, []Decision{{ToolUseID: "q1", Type: Respond, Message: "a"}}))
	if second.Reason != ReasonInterrupted || second.Err != nil {
		t.Fatalf("first resume = %+v, want interrupted again by q2", second)
	}
	resumeCompleted(t, a, second.State, Decision{ToolUseID: "q2", Type: Respond, Message: "b"})

	msgs := second.State.Messages()
	if len(msgs) != 6 || msgs[2].Content[0].ToolUseID != "q1" || msgs[2].Content[1].ToolUseID != "q2" ||
		msgs[3].Text() != "hook note" || msgs[4].Text() != "hook note" {
		t.Fatalf("history = %+v, want user, assistant, results, two hook notes, assistant", msgs)
	}
}

// dropFirstOnResume is a BeforeAgent hook that, once on is set, removes the
// oldest message, like a compaction would, and appends a note.
type dropFirstOnResume struct{ on *atomic.Bool }

func (dropFirstOnResume) Name() string { return "drop_first_on_resume" }

func (h dropFirstOnResume) BeforeAgent(_ context.Context, s *state.State) error {
	if h.on.Load() {
		s.SetMessages(append(s.Messages()[1:], llm.UserText("hook note")))
	}
	return nil
}

func TestResumeAfterHookDroppedEarlierMessages(t *testing.T) {
	log := newCallLog()
	var on atomic.Bool
	p := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w")}}, llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{writeTool(log)}, Middleware: []Middleware{dropFirstOnResume{&on}}})
	_, final := collect(t, a.Run(context.Background(), userInput("go")))

	on.Store(true)
	_, resumed := collect(t, a.Resume(context.Background(), "", final.State, []Decision{{ToolUseID: "w", Type: Approve}}))
	if resumed.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", resumed)
	}
	msgs := resumed.State.Messages()
	if len(msgs) != 4 || msgs[0].Role != llm.RoleAssistant || msgs[1].Content[0].ToolUseID != "w" || msgs[2].Text() != "hook note" {
		t.Fatalf("history = %+v, want assistant, its results, hook note, assistant", msgs)
	}
}

func TestResumeWithoutTheInterruptedMessageIsInvalid(t *testing.T) {
	log := newCallLog()
	a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log)}}, writeCall("w"))
	final.State.SetMessages([]llm.Message{llm.UserText("something else")})

	_, invalid := collect(t, a.Resume(context.Background(), "", final.State, []Decision{{ToolUseID: "w", Type: Approve}}))
	if invalid.Reason != ReasonInvalidResume || invalid.Err == nil {
		t.Fatalf("final = %+v, want invalid_resume", invalid)
	}
	if log.count("w") != 0 {
		t.Fatal("call ran although the resume was invalid")
	}
}

func TestResumedRunEventsTurnAndInvalidResumeTime(t *testing.T) {
	log := newCallLog()
	a, _, final := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(log)}}, writeCall("w"))
	events, _ := collect(t, a.Resume(context.Background(), "", final.State, []Decision{{ToolUseID: "w", Type: Approve}}))
	var sawResult bool
	for _, ev := range events {
		if ev.RunID == "" || ev.Time.IsZero() {
			t.Fatalf("resumed event %+v lacks RunID or Time", ev)
		}
		if ev.Kind == KindToolResult {
			sawResult = true
			if ev.Turn != 0 {
				t.Fatalf("resumed tool result Turn = %d, want 0 (before the first model turn)", ev.Turn)
			}
		}
	}
	if !sawResult {
		t.Fatal("no tool result from the resumed run")
	}

	events, bad := collect(t, a.Resume(context.Background(), "", state.New(), nil))
	if bad.Reason != ReasonInvalidResume || events[0].Time.IsZero() {
		t.Fatalf("invalid resume: final %+v, Time %v; want invalid_resume with a Time", bad, events[0].Time)
	}
}
