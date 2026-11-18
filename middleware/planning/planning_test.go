package planning

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

type obj = map[string]any

func registeredWriteTodos(t *testing.T) tool.Tool {
	t.Helper()
	for _, tl := range New(Config{}).(agent.ToolProvider).Tools() {
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
	res, err := registeredWriteTodos(t).Call(context.Background(), raw, &tool.Call{State: s})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	return res
}

func TestWriteTodosReplacesAndRenders(t *testing.T) {
	s := state.New()
	TodosKey.Set(s, []Todo{{Content: "old", Status: StatusPending}})

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
	TodosKey.Set(s, []Todo{{Content: "old", Status: StatusPending}})
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
	cases := map[string]struct {
		todos []obj
		want  string
	}{
		"empty content": {[]obj{{"content": "ok", "status": "pending"}, {"content": " ", "status": "pending"}}, "todo 2 has empty content"},
		"bad status":    {[]obj{{"content": "ok", "status": "x"}}, `todo 1 has invalid status "x" (use pending, in_progress, or completed)`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := callWriteTodos(t, state.New(), obj{"todos": tc.todos})
			if !res.IsError || res.Flatten() != tc.want {
				t.Fatalf("result = %q (error=%v), want error %q", res.Flatten(), res.IsError, tc.want)
			}
		})
	}
}

func TestTodosReturnsCopy(t *testing.T) {
	s := state.New()
	TodosKey.Set(s, []Todo{{Content: "a", Status: StatusPending}})
	Todos(s)[0].Content = "changed"
	if Todos(s)[0].Content != "a" {
		t.Fatal("mutating the returned slice changed the state")
	}
}

// noopInput is the input of the no-op tool that fills turns in the cadence test.
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

	a, err := agent.New(agent.Config{Provider: p, Tools: []tool.Tool{noopTool()}, Middleware: []agent.Middleware{New(Config{})}})
	if err != nil {
		t.Fatal(err)
	}
	var final *agent.Final
	for ev, err := range a.Run(context.Background(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}) {
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
	if text := p.Requests[11].Messages[len(p.Requests[11].Messages)-1].Text(); text != want {
		t.Fatalf("reminder = %q, want %q", text, want)
	}
	for _, m := range final.State.Messages() {
		if strings.Contains(m.Text(), "<system-reminder>") {
			t.Fatal("the reminder must be view only, but it was persisted in State")
		}
	}
}

func TestNoReminderWhenAllCompleted(t *testing.T) {
	m := New(Config{ReminderAfter: 1, ReminderEvery: 1})
	s := state.New()
	TodosKey.Set(s, []Todo{{Content: "a", Status: StatusCompleted}})
	s.SetMessages([]llm.Message{llm.UserText("go"), {Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("x")}}, llm.UserText("more")})

	var seen llm.Request
	req := &agent.ModelRequest{State: s, Request: llm.Request{Messages: s.Messages()}}
	_, _ = m.(agent.ModelWrapper).WrapModel(context.Background(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
		seen = r.Request
		return &llm.Response{}, nil
	})
	if hasReminder(seen) {
		t.Fatal("no reminder expected when every todo is completed")
	}
}
