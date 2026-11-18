package agent

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// infoLog records the RunInfo seen at named points of a run.
type infoLog struct {
	mu   sync.Mutex
	seen map[string][]RunInfo
	// missing lists the points where Info reported no run.
	missing []string
}

func newInfoLog() *infoLog { return &infoLog{seen: map[string][]RunInfo{}} }

func (l *infoLog) record(ctx context.Context, at string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	info, ok := Info(ctx)
	if !ok {
		l.missing = append(l.missing, at)
		return
	}
	l.seen[at] = append(l.seen[at], info)
}

// only returns the single RunInfo recorded at the given point.
func (l *infoLog) only(t *testing.T, at string) RunInfo {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.missing) > 0 {
		t.Fatalf("Info reported no run at %v", l.missing)
	}
	if len(l.seen[at]) != 1 {
		t.Fatalf("Info recorded %d times at %s, want once", len(l.seen[at]), at)
	}
	return l.seen[at][0]
}

// infoProbe records Info in BeforeAgent and around the model call.
type infoProbe struct{ log *infoLog }

func (infoProbe) Name() string { return "info_probe" }

func (p infoProbe) BeforeAgent(ctx context.Context, _ *state.State) error {
	p.log.record(ctx, "BeforeAgent")
	return nil
}

func (p infoProbe) WrapModel(ctx context.Context, req *ModelRequest, next ModelHandler) (*llm.Response, error) {
	p.log.record(ctx, "WrapModel")
	return next(ctx, req)
}

// pauseOnceTool records Info, interrupts on its first invocation and
// succeeds once resumed.
func pauseOnceTool(log *infoLog) tool.Tool {
	return tool.New("pause_once", "Interrupt until resumed.",
		func(ctx context.Context, _ struct{}, c *tool.Call) (tool.Result, error) {
			log.record(ctx, "tool")
			if c.Resume == nil {
				return tool.Result{}, &Interrupt{}
			}
			return tool.Text("resumed"), nil
		},
		tool.WithReadOnly())
}

func TestInfoInHooksAndTools(t *testing.T) {
	first := newInfoLog()
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "p1", Name: "pause_once"}}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{pauseOnceTool(first)}, Middleware: []Middleware{infoProbe{first}}})
	input := []llm.Message{llm.UserText("go")}

	_, final := collect(t, a.Run(context.Background(), Input{ThreadID: "t", Messages: input}))

	if final.Reason != ReasonInterrupted {
		t.Fatalf("final = %+v, want interrupted", final)
	}
	for _, at := range []string{"BeforeAgent", "WrapModel", "tool"} {
		info := first.only(t, at)
		if info.Agent != a || info.ThreadID != "t" || info.AgentName != DefaultAgentName || info.Depth != 0 ||
			info.ParentToolID != "" || info.Resumed || !reflect.DeepEqual(info.Input, input) {
			t.Fatalf("Info at %s = %+v, want the top-level run of thread t with its input", at, info)
		}
	}

	resumed := newInfoLog()
	a = mustNew(t, Config{Provider: p, Tools: []tool.Tool{pauseOnceTool(resumed)}, Middleware: []Middleware{infoProbe{resumed}}})
	_, final = collect(t, a.Resume(context.Background(), "t", final.State, []Decision{{ToolUseID: "p1", Type: Approve}}))

	if final.Reason != ReasonCompleted {
		t.Fatalf("resumed final = %+v, want completed", final)
	}
	for _, at := range []string{"BeforeAgent", "WrapModel", "tool"} {
		info := resumed.only(t, at)
		if info.ThreadID != "t" || !info.Resumed || info.Input != nil || info.Depth != 0 {
			t.Fatalf("Info at %s of the resumed run = %+v, want Resumed with nil Input", at, info)
		}
	}
}

func TestInfoOutsideRun(t *testing.T) {
	if info, ok := Info(context.Background()); ok {
		t.Fatalf("Info outside a run = %+v, true; want false", info)
	}
	if Forward(context.Background(), Event{Kind: KindText, Text: "x"}) {
		t.Fatal("Forward outside a tool call reported success")
	}
}

// forwardProbe records whether Forward succeeds from BeforeAgent, which runs
// outside any tool call of its own run.
type forwardProbe struct{ forwarded *bool }

func (forwardProbe) Name() string { return "forward_probe" }

func (p forwardProbe) BeforeAgent(ctx context.Context, _ *state.State) error {
	*p.forwarded = Forward(ctx, Event{Kind: KindText, Text: "from child hook"})
	return nil
}

// nestedOutcome is what the delegating tool of TestNestedRunDepthAndForward
// observed.
type nestedOutcome struct {
	toolCtx        context.Context
	finalForwarded bool
	hookForwarded  bool
}

// delegateTool runs child inside its call and forwards every child event,
// plus one already forwarded by a deeper run.
func delegateTool(child *Agent, out *nestedOutcome) tool.Tool {
	return tool.New("delegate", "Run the child agent.",
		func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
			out.toolCtx = ctx
			for ev := range child.Run(ctx, Input{ThreadID: "child-thread", Messages: []llm.Message{llm.UserText("hi")}}) {
				ok := Forward(ctx, ev)
				if ev.Kind == KindFinal {
					out.finalForwarded = ok
				}
			}
			Forward(ctx, Event{Kind: KindText, Agent: "grandchild", ParentToolID: "inner", Text: "deep"})
			return tool.Text("delegated"), nil
		},
		tool.WithReadOnly())
}

func TestNestedRunDepthAndForward(t *testing.T) {
	childInfo := newInfoLog()
	var out nestedOutcome
	child := mustNew(t, Config{
		Name:       "child",
		Provider:   llmtest.New(llmtest.Turn{Text: "hello from child"}),
		Middleware: []Middleware{infoProbe{childInfo}, forwardProbe{&out.hookForwarded}},
	})
	parent := mustNew(t, Config{
		Provider: llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "d1", Name: "delegate"}}}, llmtest.Turn{Text: "done"}),
		Tools:    []tool.Tool{delegateTool(child, &out)},
	})

	events, final := collect(t, parent.Run(context.Background(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	info := childInfo.only(t, "BeforeAgent")
	if info.Depth != 1 || info.ParentToolID != "d1" || info.AgentName != "child" || info.ThreadID != "child-thread" {
		t.Fatalf("child Info = %+v, want depth 1 under call d1", info)
	}
	var childText, deep bool
	for _, ev := range events {
		switch {
		case ev.Agent == "child":
			if ev.Kind == KindFinal {
				t.Fatal("child KindFinal was forwarded")
			}
			if ev.ParentToolID != "d1" {
				t.Fatalf("child event %+v, want ParentToolID d1", ev)
			}
			childText = childText || (ev.Kind == KindText && ev.Text == "hello from child")
		case ev.Agent == "grandchild":
			deep = ev.ParentToolID == "inner" && ev.Text == "deep"
		case ev.Agent != DefaultAgentName || ev.ParentToolID != "":
			t.Fatalf("parent event %+v, want Agent main and no ParentToolID", ev)
		}
	}
	if !childText {
		t.Fatal("parent stream has no forwarded child text")
	}
	if !deep {
		t.Fatal("an event forwarded from a deeper run lost its ParentToolID or Agent")
	}
	if out.finalForwarded {
		t.Fatal("Forward reported success for KindFinal")
	}
	if out.hookForwarded {
		t.Fatal("Forward from the child's BeforeAgent reached the parent: it is not inside a child tool call")
	}
	if Forward(out.toolCtx, Event{Kind: KindText, Text: "late"}) {
		t.Fatal("Forward succeeded after the parent run ended")
	}
}

// requestProbe records the backend and thread id of every model request.
type requestProbe struct {
	backends *[]backend.Backend
	threads  *[]string
}

func (requestProbe) Name() string { return "request_probe" }

func (p requestProbe) WrapModel(ctx context.Context, req *ModelRequest, next ModelHandler) (*llm.Response, error) {
	*p.backends = append(*p.backends, req.Backend)
	*p.threads = append(*p.threads, req.ThreadID)
	return next(ctx, req)
}

// envTool records the tool.Call it receives.
func envTool(seen *[]tool.Call) tool.Tool {
	return tool.New("env", "Record the call environment.",
		func(_ context.Context, _ struct{}, c *tool.Call) (tool.Result, error) {
			*seen = append(*seen, *c)
			return tool.Text("ok"), nil
		},
		tool.WithReadOnly())
}

func TestInputBackendOverrides(t *testing.T) {
	override := memory.New()
	var backends []backend.Backend
	var threads []string
	var calls []tool.Call
	a := mustNew(t, Config{
		Provider:       llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e", Name: "env"}}}, llmtest.Turn{Text: "done"}),
		Tools:          []tool.Tool{envTool(&calls)},
		Middleware:     []Middleware{requestProbe{&backends, &threads}},
		BackendFactory: func(string) backend.Backend { return memory.New() },
	})

	_, final := collect(t, a.Run(context.Background(), Input{ThreadID: "t", Backend: override, Messages: []llm.Message{llm.UserText("go")}}))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	if len(calls) != 1 || calls[0].Backend != override {
		t.Fatalf("tool backends = %+v, want the Input.Backend", calls)
	}
	for i := range backends {
		if backends[i] != override || threads[i] != "t" {
			t.Fatalf("model request %d: backend %v, thread %q; want Input.Backend and thread t", i, backends[i], threads[i])
		}
	}
	if len(backends) != 2 {
		t.Fatalf("saw %d model requests, want 2", len(backends))
	}
}

func TestInputAgentName(t *testing.T) {
	var calls []tool.Call
	log := newInfoLog()
	a := mustNew(t, Config{
		Name:       "worker",
		Provider:   llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "e", Name: "env"}}}, llmtest.Turn{Text: "done"}),
		Tools:      []tool.Tool{envTool(&calls)},
		Middleware: []Middleware{infoProbe{log}},
	})

	in := userInput("go")
	in.AgentName = "alias"
	events, _ := collect(t, a.Run(context.Background(), in))

	for _, ev := range events {
		if ev.Agent != "alias" {
			t.Fatalf("event %s has Agent %q, want alias", ev.Kind, ev.Agent)
		}
	}
	if len(calls) != 1 || calls[0].AgentID != "alias" {
		t.Fatalf("tool AgentID = %+v, want alias", calls)
	}
	if info := log.only(t, "BeforeAgent"); info.AgentName != "alias" {
		t.Fatalf("Info.AgentName = %q, want alias", info.AgentName)
	}
}

func TestResumeWithInputOverrides(t *testing.T) {
	override := memory.New()
	var backends []backend.Backend
	var threads []string
	var envCalls []tool.Call
	log := newInfoLog()
	a := mustNew(t, Config{
		Provider: llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "p1", Name: "pause_once"}, {ID: "e", Name: "env"}}},
			llmtest.Turn{Text: "done"},
		),
		Tools:          []tool.Tool{pauseOnceTool(log), envTool(&envCalls)},
		Middleware:     []Middleware{requestProbe{&backends, &threads}},
		BackendFactory: func(string) backend.Backend { return memory.New() },
	})
	_, interrupted := collect(t, a.Run(context.Background(), Input{ThreadID: "t", Messages: []llm.Message{llm.UserText("go")}}))
	if interrupted.Reason != ReasonInterrupted {
		t.Fatalf("final = %+v, want interrupted", interrupted)
	}
	backends, threads = nil, nil

	in := Input{ThreadID: "t", State: interrupted.State, Backend: override, AgentName: "resumer"}
	events, final := collect(t, a.ResumeWith(context.Background(), in, []Decision{{ToolUseID: "p1", Type: Approve}}))

	if final.Reason != ReasonCompleted {
		t.Fatalf("resumed final = %+v, want completed", final)
	}
	if len(envCalls) != 1 || envCalls[0].Backend != override || envCalls[0].AgentID != "resumer" {
		t.Fatalf("resumed tool call = %+v, want Input.Backend and AgentID resumer", envCalls)
	}
	if len(backends) != 1 || backends[0] != override || threads[0] != "t" {
		t.Fatalf("resumed model requests: backends %v, threads %v; want Input.Backend on thread t", backends, threads)
	}
	for _, ev := range events {
		if ev.Agent != "resumer" {
			t.Fatalf("event %s has Agent %q, want resumer", ev.Kind, ev.Agent)
		}
	}
}

func TestResumeWithInvalidKeepsAgentName(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New()})

	events, final := collect(t, a.ResumeWith(context.Background(), Input{AgentName: "resumer"}, nil))

	if final.Reason != ReasonInvalidResume || events[0].Agent != "resumer" {
		t.Fatalf("event = %+v, want invalid resume from agent resumer", events[0])
	}
}

func TestResumeWithRejectsMessages(t *testing.T) {
	a, _, interrupted := interruptedRun(t, Config{Tools: []tool.Tool{writeTool(newCallLog())}}, writeCall("w"))
	before := len(interrupted.State.Messages())

	in := Input{State: interrupted.State, Messages: []llm.Message{llm.UserText("more")}}
	_, final := collect(t, a.ResumeWith(context.Background(), in, []Decision{{ToolUseID: "w", Type: Approve}}))

	if final.Reason != ReasonInvalidResume || final.Err == nil {
		t.Fatalf("final = %+v, want invalid resume", final)
	}
	if got := len(final.State.Messages()); got != before {
		t.Fatalf("state has %d messages, want %d: an invalid resume leaves it untouched", got, before)
	}
}
