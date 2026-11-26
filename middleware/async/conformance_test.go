package async_test

import (
	"context"
	"iter"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/async/asynctest"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// blocking is a provider whose every request waits for its context to end.
type blocking struct{}

func (blocking) Stream(ctx context.Context, _ llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		<-ctx.Done()
		yield(llm.StreamEvent{}, ctx.Err())
	}
}

func (b blocking) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, b, req)
}

// capture is a Runner that starts tasks with inner and keeps their Handles.
type capture struct {
	inner async.Runner

	mu      sync.Mutex
	handles []async.Handle
}

func (c *capture) Start(ctx context.Context, req async.Request) (async.Handle, error) {
	h, err := c.inner.Start(ctx, req)
	if err == nil {
		c.mu.Lock()
		c.handles = append(c.handles, h)
		c.mu.Unlock()
	}
	return h, err
}

// startLocal starts one task of the built-in runner for sc through a parent
// agent's start_task, and returns the runner's Handle for it.
func startLocal(t *testing.T, sc asynctest.Scenario) async.Handle {
	t.Helper()
	spec := subagents.Subagent{Name: "worker", Description: "Works.", SystemPrompt: "you work"}
	switch sc {
	case asynctest.ScenarioComplete:
		spec.Provider = llmtest.New(llmtest.Turn{Text: "the answer"})
	case asynctest.ScenarioRunUntilStopped:
		spec.Provider = blocking{}
	case asynctest.ScenarioWait:
		// write_file needs approval in the default mode, and nobody can
		// give it in-band: the task stops waiting.
		spec.Provider = llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w1", Name: "write_file", Input: map[string]any{}}}})
		spec.Tools = []tool.Tool{tool.New("write_file", "Writes.", func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
			return tool.Text("written"), nil
		})}
	}
	cfg := async.Config{Subagents: []subagents.Subagent{spec}, DisableGeneralPurpose: true, DisableStopReminder: true}
	local, err := async.NewLocalRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runner := &capture{inner: local}
	cfg.Runner = runner
	m, err := async.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	parent := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s1", Name: async.ToolStart, Input: map[string]any{"subagent_type": "worker", "description": "do it"}}}},
		llmtest.Turn{Text: "started"},
	)
	a, err := agent.New(agent.Config{Provider: parent, Middleware: []agent.Middleware{m}, PermissionMode: permission.ModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	in := agent.Input{ThreadID: "t", State: state.New(), Backend: testutil.Memory(t), Messages: []llm.Message{llm.UserText("go")}}
	var final *agent.Final
	for ev := range a.Run(t.Context(), in) {
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Fatalf("parent run: %+v", final)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.handles) != 1 {
		t.Fatalf("%d tasks started, want 1", len(runner.handles))
	}
	return runner.handles[0]
}

func TestLocalRunnerConformance(t *testing.T) {
	asynctest.Run(t, startLocal)
}
