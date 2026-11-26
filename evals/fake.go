package evals

import (
	"context"
	"encoding/json"
	"iter"
	"slices"
	"strings"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	membackend "github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// fakeShell is an in-memory workspace whose execute tool runs nothing: it
// records each command and answers with a canned output. Scenarios that
// tempt the model into a destructive command use it, so a harness bug can
// never do damage.
type fakeShell struct {
	*membackend.Backend

	mu   sync.Mutex
	cmds []string
}

var _ backend.Executor = (*fakeShell)(nil)

// newFakeShell returns a fakeShell over a new memory backend. The memory
// backend cannot fail with its zero config, so an error panics.
func newFakeShell() *fakeShell {
	mem, err := membackend.New(membackend.Config{})
	if err != nil {
		panic("evals: memory backend with the zero config: " + err.Error()) // cannot happen
	}
	return &fakeShell{Backend: mem}
}

// Execute implements backend.Executor.
func (f *fakeShell) Execute(_ context.Context, cmd string, _ backend.ExecOptions) (backend.ExecResult, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.mu.Unlock()
	out := "ok\n"
	if rest, ok := strings.CutPrefix(strings.TrimSpace(cmd), "echo "); ok {
		out = strings.Trim(rest, `"'`) + "\n"
	}
	return backend.ExecResult{Output: out}, nil
}

// commands returns every command executed so far.
func (f *fakeShell) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cmds)
}

// approvals is a permission.Approver that records every question and
// declines it.
type approvals struct {
	mu   sync.Mutex
	reqs []permission.Request
}

func (a *approvals) decline(_ context.Context, req permission.Request, _ string) permission.Decision {
	a.mu.Lock()
	a.reqs = append(a.reqs, req)
	a.mu.Unlock()
	return permission.Denied("the user declined this action")
}

// asked returns the subjects of every question about tool.
func (a *approvals) asked(toolName string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var subjects []string
	for _, r := range a.reqs {
		if r.Tool == toolName {
			subjects = append(subjects, r.Subject)
		}
	}
	return subjects
}

// drain consumes a run. The final event comes with Final.Err as its error,
// which the scenario judges by the final's reason; any other stream error
// is recorded on t.
func drain(t *T, run iter.Seq2[agent.Event, error]) (*agent.Final, []agent.Event) {
	var final *agent.Final
	var events []agent.Event
	for ev, err := range run {
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		} else if err != nil {
			t.Errorf("stream error: %v", err)
			continue
		}
		events = append(events, ev)
	}
	return final, events
}

// snippet shortens s for a failure message.
func snippet(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 160 {
		return string(r[:160]) + "..."
	}
	return s
}

// toolNamesOf lists the names of calls, in order.
func toolNamesOf(calls []*toolCall) []string {
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Name
	}
	return names
}

// observe records final on t and reports whether the run ended in a way the
// scenario can judge: a model failure is an error, not a verdict.
func observe(t *T, final *agent.Final) bool {
	t.Observe(final)
	if final == nil {
		return false
	}
	switch final.Reason {
	case agent.ReasonModelError, agent.ReasonAbortedStreaming, agent.ReasonAbortedTools:
		t.Errorf("run ended with %s: %v", final.Reason, final.Err)
		return false
	}
	return true
}

// toolCall is one tool use seen in a run's events and its result.
type toolCall struct {
	ID     string
	Name   string
	Input  json.RawMessage
	Result *tool.Result
}

// toolCalls pairs the tool uses of events with their results, in order. A
// forwarded subagent event is left out.
func toolCalls(events []agent.Event) []*toolCall {
	var calls []*toolCall
	byID := map[string]*toolCall{}
	for _, ev := range events {
		if ev.ParentToolID != "" {
			continue
		}
		switch ev.Kind {
		case agent.KindToolUse:
			c := &toolCall{ID: ev.ToolID, Name: ev.ToolName, Input: ev.Input}
			calls = append(calls, c)
			byID[ev.ToolID] = c
		case agent.KindToolResult:
			if c, ok := byID[ev.ToolID]; ok {
				c.Result = ev.ToolResult
			}
		}
	}
	return calls
}

// named returns the calls of tool name.
func named(calls []*toolCall, name string) []*toolCall {
	var out []*toolCall
	for _, c := range calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// command returns the command of an execute input, empty when there is none.
func command(input json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	return in.Command
}

// resultText joins the text blocks of r.
func resultText(r *tool.Result) string {
	if r == nil {
		return ""
	}
	var parts []string
	for _, b := range r.Content {
		if b.Type == llm.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
