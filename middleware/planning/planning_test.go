package planning

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

type obj = map[string]any

func registeredWriteTodos(t *testing.T) tool.Tool {
	t.Helper()
	for _, tl := range mustNew(t, Config{}).Tools() {
		if tl.Name() == ToolWriteTodos {
			return tl
		}
	}
	t.Fatal("write_todos not registered")
	return nil
}

func callWriteTodos(t *testing.T, s *state.State, input any) tool.Result {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	res, err := registeredWriteTodos(t).Call(t.Context(), raw, &tool.Call{State: s})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	return res
}

func TestWriteTodosReplacesAndRenders(t *testing.T) {
	s := state.New()
	todosKey.Set(s, []Todo{{Content: "old", Status: StatusPending}})

	res := callWriteTodos(t, s, obj{"todos": []obj{
		{"content": "design", "status": "completed"},
		{"content": "build", "status": "in_progress"},
		{"content": "ship", "status": "pending"},
	}})

	want := "Todo list updated:\n[x] design\n[~] build\n[ ] ship"
	if res.IsError || res.Flatten() != want {
		t.Fatalf("result = %q (error=%v), want %q", res.Flatten(), res.IsError, want)
	}
	got := Todos(s)
	if len(got) != 3 || got[0].Content != "design" || got[1].Status != StatusInProgress {
		t.Fatalf("Todos = %+v, want the new list replacing the old one", got)
	}
}

func TestWriteTodosAllowsEmptyList(t *testing.T) {
	s := state.New()
	todosKey.Set(s, []Todo{{Content: "old", Status: StatusPending}})
	res := callWriteTodos(t, s, obj{"todos": []obj{}})
	if res.IsError || len(Todos(s)) != 0 {
		t.Fatalf("empty list should clear the plan: error=%v todos=%+v", res.IsError, Todos(s))
	}
}

func TestWriteTodosRejectsTwoInProgress(t *testing.T) {
	s := state.New()
	res := callWriteTodos(t, s, obj{"todos": []obj{
		{"content": "a", "status": "in_progress"},
		{"content": "b", "status": "in_progress"},
	}})
	if !res.IsError || res.Flatten() != "only one todo may be in_progress at a time" {
		t.Fatalf("result = %q (error=%v)", res.Flatten(), res.IsError)
	}
	if len(Todos(s)) != 0 {
		t.Fatal("a rejected update must not change the list")
	}
}

func TestWriteTodosRejectsBadItems(t *testing.T) {
	tests := map[string]struct {
		todos []obj
		want  string
	}{
		"empty content": {[]obj{{"content": "ok", "status": "pending"}, {"content": " ", "status": "pending"}}, "todo 2 has empty content"},
		"bad status":    {[]obj{{"content": "ok", "status": "x"}}, `todo 1 has invalid status "x" (use pending, in_progress, or completed)`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			res := callWriteTodos(t, state.New(), obj{"todos": tt.todos})
			if !res.IsError || res.Flatten() != tt.want {
				t.Fatalf("result = %q (error=%v), want error %q", res.Flatten(), res.IsError, tt.want)
			}
		})
	}
}

func TestTodosReturnsCopy(t *testing.T) {
	s := state.New()
	todosKey.Set(s, []Todo{{Content: "a", Status: StatusPending}})
	Todos(s)[0].Content = "changed"
	if Todos(s)[0].Content != "a" {
		t.Fatal("mutating the returned slice changed the state")
	}
}

// noopInput is the input of the no-op tool that fills turns in the cadence
// test.
type noopInput struct{}

func noopTool() tool.Tool {
	return tool.New("noop", "Does nothing.",
		func(context.Context, noopInput, *tool.Call) (tool.Result, error) { return tool.Text("ok"), nil },
		tool.WithReadOnly(), tool.WithConcurrent())
}

func hasReminder(req llm.Request) bool {
	last := req.Messages[len(req.Messages)-1]
	return last.Role == llm.RoleUser && strings.HasPrefix(last.Text(), "<system-reminder>Your todo list:\n")
}

func TestReminderCadence(t *testing.T) {
	turns := []llmtest.Turn{{ToolCalls: []llmtest.Call{{ID: "w", Name: ToolWriteTodos, Input: obj{"todos": []obj{
		{"content": "step one", "status": "in_progress"},
		{"content": "step two", "status": "pending"},
	}}}}}}
	for i := 1; i < 25; i++ {
		turns = append(turns, llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "n" + string(rune('a'+i)), Name: "noop", Input: obj{}}}})
	}
	turns = append(turns, llmtest.Turn{Text: "done"})
	p := llmtest.New(turns...)

	a, err := agent.New(agent.Config{Provider: p, Tools: []tool.Tool{noopTool()}, Middleware: []agent.Middleware{mustNew(t, Config{})}})
	if err != nil {
		t.Fatal(err)
	}
	var final *agent.Final
	for ev, err := range a.Run(t.Context(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil || final.Text != "done" {
		t.Fatalf("final = %+v, want completed with text done", final)
	}

	var got []int
	for i, req := range p.Requests {
		if hasReminder(req) {
			got = append(got, i)
		}
	}
	if want := []int{11, 21}; !slices.Equal(got, want) {
		t.Fatalf("reminders at request indexes %v, want %v", got, want)
	}
	want := "<system-reminder>Your todo list:\n[~] step one\n[ ] step two\nKeep it current with write_todos.</system-reminder>"
	reminder := p.Requests[11].Messages[len(p.Requests[11].Messages)-1]
	if reminder.Text() != want {
		t.Fatalf("reminder = %q, want %q", reminder.Text(), want)
	}
	if reminder.Meta[agent.InjectedMetaKey] != "planning" {
		t.Fatalf("reminder meta = %v, want it marked as injected", reminder.Meta)
	}
	for _, m := range final.State.Messages() {
		if strings.Contains(m.Text(), "<system-reminder>") {
			t.Fatal("the reminder must be view only, but it was persisted in State")
		}
	}
}

func TestNoReminderWhenAllCompleted(t *testing.T) {
	m := mustNew(t, Config{ReminderAfter: 1, ReminderEvery: 1})
	s := state.New()
	todosKey.Set(s, []Todo{{Content: "a", Status: StatusCompleted}})
	s.SetMessages([]llm.Message{llm.UserText("go"), {Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("x")}}, llm.UserText("more")})

	var seen llm.Request
	req := &agent.ModelRequest{State: s, Request: llm.Request{Messages: s.Messages()}}
	_, _ = m.WrapModel(t.Context(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
		seen = r.Request
		return &llm.Response{}, nil
	})
	if hasReminder(seen) {
		t.Fatal("no reminder expected when every todo is completed")
	}
}

func TestWriteTodosIsReadOnly(t *testing.T) {
	if !testutil.IsReadOnly(registeredWriteTodos(t), json.RawMessage(`{"todos":[]}`)) {
		t.Fatal("write_todos is not read-only, so plan and dontAsk modes refuse it")
	}
}

func TestTodosKeyIsNamespaced(t *testing.T) {
	s := state.New()
	callWriteTodos(t, s, obj{"todos": []obj{{"content": "a", "status": "pending"}}})
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"planning.todos"`) {
		t.Fatalf("serialized state %s lacks the planning.todos key", data)
	}
}

func TestNewRejectsNegative(t *testing.T) {
	for name, cfg := range map[string]Config{
		"ReminderAfter": {ReminderAfter: -1},
		"ReminderEvery": {ReminderEvery: -1},
	} {
		m, err := New(cfg)
		if err == nil || !strings.HasPrefix(err.Error(), "planning: ") || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: New = %v, %v; want a planning error naming the field", name, m, err)
		}
	}
}

func TestName(t *testing.T) {
	if Name != "planning" || mustNew(t, Config{}).Name() != Name {
		t.Fatalf("Name = %q", Name)
	}
}

func TestWriteTodosInputExported(t *testing.T) {
	want := []Todo{{Content: "a", Status: StatusInProgress}, {Content: "b", Status: StatusPending}}
	raw, err := json.Marshal(WriteTodosInput{Todos: want})
	if err != nil {
		t.Fatal(err)
	}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "w", Name: ToolWriteTodos, Input: json.RawMessage(raw)}}},
		llmtest.Turn{Text: "done"},
	)
	a, err := agent.New(agent.Config{Provider: p, Middleware: []agent.Middleware{mustNew(t, Config{})}})
	if err != nil {
		t.Fatal(err)
	}
	var final *agent.Final
	for ev, err := range a.Run(t.Context(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatal("no final event")
	}
	if got := Todos(final.State); !slices.Equal(got, want) {
		t.Fatalf("todos = %v, want %v", got, want)
	}
}
