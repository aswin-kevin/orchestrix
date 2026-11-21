package agent_test

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// ledger records the result of every tool call it sees. As a ToolWrapper it
// answers a call it already recorded without running it again; as an
// OrphanResolver it answers a call a crash left without a result from what it
// recorded, and says the call can be retried when it recorded nothing.
type ledger struct {
	mu   sync.Mutex
	done map[string]tool.Result
}

func (l *ledger) Name() string { return "ledger" }

func (l *ledger) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	l.mu.Lock()
	res, ok := l.done[call.ID]
	l.mu.Unlock()
	if ok {
		return res, nil
	}
	res, err := next(ctx, call)
	if err == nil {
		l.mu.Lock()
		l.done[call.ID] = res
		l.mu.Unlock()
	}
	return res, err
}

func (l *ledger) ResolveOrphan(_ context.Context, _ *state.State, o agent.Orphan) (tool.Result, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if res, ok := l.done[o.ID]; ok {
		return res, true, nil
	}
	return tool.Errorf("not run; safe to retry"), true, nil
}

// A process crashed after the model asked for two tool calls. The ledger
// knows the first one ran and has no record of the second, so the next run
// stores their results in State before the new prompt.
func ExampleOrphanResolver() {
	l := &ledger{done: map[string]tool.Result{"c1": tool.Text("deployed v7")}}
	a, err := agent.New(agent.Config{
		Provider:   llmtest.New(llmtest.Turn{Text: "ok"}),
		Middleware: []agent.Middleware{l},
	})
	if err != nil {
		log.Fatal(err)
	}

	// State as the crash left it: two calls, no results.
	s := state.New()
	s.SetMessages([]llm.Message{
		llm.UserText("deploy, then notify"),
		{Role: llm.RoleAssistant, Content: []llm.Block{
			{Type: llm.BlockToolUse, ID: "c1", Name: "deploy", Input: []byte(`{}`)},
			{Type: llm.BlockToolUse, ID: "c2", Name: "notify", Input: []byte(`{}`)},
		}},
	})

	var final *agent.Final
	for ev, err := range a.Run(context.Background(), agent.Input{
		State:    s,
		Messages: []llm.Message{llm.UserText("where were we?")},
	}) {
		if err != nil {
			log.Fatal(err)
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	for _, b := range final.State.Messages()[2].Content {
		fmt.Printf("%s: %s\n", b.ToolUseID, b.Content[0].Text)
	}
	// Output:
	// c1: deployed v7
	// c2: not run; safe to retry
}
