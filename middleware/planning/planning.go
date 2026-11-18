// Package planning gives the agent a todo list it maintains itself. The
// write_todos tool replaces the list, the list lives in private run state, and
// when the agent stops touching a still-open list for a while the middleware
// nudges it with a reminder that is added to the model request only.
package planning

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// ToolWriteTodos is the name of the tool that replaces the todo list.
const ToolWriteTodos = "write_todos"

// Defaults applied when the matching Config field is zero.
const (
	// DefaultReminderAfter is how many assistant turns may pass after the
	// last write_todos call before the first reminder.
	DefaultReminderAfter = 10
	// DefaultReminderEvery is the minimum number of assistant turns between
	// two reminders.
	DefaultReminderEvery = 10
)

// Status is the progress of one todo.
type Status string

// The statuses a todo can have.
const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// Todo is one item of the plan.
type Todo struct {
	Content string `json:"content" desc:"What needs to be done, in a short imperative sentence."`
	Status  Status `json:"status" desc:"One of pending, in_progress or completed."`
}

// TodosKey holds the current todo list. It is private to the run that owns it.
var TodosKey = state.NewKey[[]Todo]("planning.todos", state.Private[[]Todo]())

// remindedTurnKey records the assistant-turn count at which the last reminder
// was added, so reminders keep their spacing.
var remindedTurnKey = state.NewKey[int]("planning.reminded_turn", state.Private[int]())

// Todos returns a copy of the current todo list; it is empty when none was set.
func Todos(s *state.State) []Todo {
	todos, _ := TodosKey.Get(s)
	return slices.Clone(todos)
}

// Config configures the planning middleware.
type Config struct {
	// ReminderAfter is the number of assistant turns since the last
	// write_todos call after which a reminder is added; 0 means
	// DefaultReminderAfter.
	ReminderAfter int
	// ReminderEvery is the minimum number of assistant turns between
	// reminders; 0 means DefaultReminderEvery.
	ReminderEvery int
	// Prompt is the system prompt segment describing write_todos. nil uses
	// DefaultPrompt; a pointer to "" disables the segment.
	Prompt *string
}

// middleware is the planning middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type middleware struct {
	reminderAfter, reminderEvery int
	prompt                       string
}

// New returns the planning middleware.
func New(cfg Config) agent.Middleware {
	m := &middleware{
		reminderAfter: DefaultReminderAfter,
		reminderEvery: DefaultReminderEvery,
		prompt:        DefaultPrompt,
	}
	if cfg.ReminderAfter > 0 {
		m.reminderAfter = cfg.ReminderAfter
	}
	if cfg.ReminderEvery > 0 {
		m.reminderEvery = cfg.ReminderEvery
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	return m
}

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "planning" }

// Tools implements agent.ToolProvider.
func (m *middleware) Tools() []tool.Tool { return []tool.Tool{writeTodosTool()} }

type writeTodosInput struct {
	Todos []Todo `json:"todos" desc:"The complete todo list. It replaces the previous list; send an empty list to clear it."`
}

func writeTodosTool() tool.Tool {
	return tool.New(ToolWriteTodos,
		"Replace your todo list. Send the complete list every time, with each item's current status. At most one item may be in_progress.",
		writeTodos,
		// The tool changes nothing but the run's own private state, never the
		// workspace or the outside world, so it counts as read-only. That lets
		// it run without approval in every mode, plan and dontAsk included,
		// while an explicit ask or deny rule for it still applies.
		tool.WithReadOnly(),
	)
}

func writeTodos(_ context.Context, in writeTodosInput, c *tool.Call) (tool.Result, error) {
	if err := validate(in.Todos); err != nil {
		return tool.Errorf("%v", err), nil
	}
	todos := slices.Clone(in.Todos)
	TodosKey.Set(c.State, todos)
	return tool.Text("Todo list updated:\n" + render(todos)), nil
}

// validate checks a proposed list; its error text goes back to the model.
func validate(todos []Todo) error {
	inProgress := 0
	for i, t := range todos {
		n := i + 1
		if strings.TrimSpace(t.Content) == "" {
			return fmt.Errorf("todo %d has empty content", n)
		}
		switch t.Status {
		case StatusPending, StatusCompleted:
		case StatusInProgress:
			inProgress++
		default:
			return fmt.Errorf("todo %d has invalid status %q (use pending, in_progress, or completed)", n, t.Status)
		}
	}
	if inProgress > 1 {
		return fmt.Errorf("only one todo may be in_progress at a time")
	}
	return nil
}

// render writes one line per todo, prefixed by a marker for its status.
func render(todos []Todo) string {
	lines := make([]string, len(todos))
	for i, t := range todos {
		lines[i] = marker(t.Status) + " " + t.Content
	}
	return strings.Join(lines, "\n")
}

func marker(s Status) string {
	switch s {
	case StatusInProgress:
		return "[~]"
	case StatusCompleted:
		return "[x]"
	}
	return "[ ]"
}

// WrapModel implements agent.ModelWrapper. It appends the prompt after the
// host's system segments, so it stays outside the cached prefix, and adds the
// reminder to the request view when one is due.
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if m.prompt != "" {
		req.Request.System = append(slices.Clone(req.Request.System), m.prompt)
	}
	if reminder, ok := m.dueReminder(req.State); ok {
		msg := llm.UserText(reminder)
		msg.Meta = map[string]string{agent.InjectedMetaKey: "planning"}
		req.Request.Messages = append(slices.Clone(req.Request.Messages), msg)
	}
	return next(ctx, req)
}

// dueReminder returns the reminder text when the list is open and the agent
// has neither updated it nor been reminded for long enough. Handing out a
// reminder records the turn, which is what spaces the next one.
func (m *middleware) dueReminder(s *state.State) (string, bool) {
	todos := Todos(s)
	if len(todos) == 0 || allCompleted(todos) {
		return "", false
	}
	turns, sinceWrite := assistantTurns(s.Messages())
	if sinceWrite < m.reminderAfter {
		return "", false
	}
	if last, ok := remindedTurnKey.Get(s); ok && turns-last < m.reminderEvery {
		return "", false
	}
	remindedTurnKey.Set(s, turns)
	return "<system-reminder>Your todo list:\n" + render(todos) + "\nKeep it current with write_todos.</system-reminder>", true
}

func allCompleted(todos []Todo) bool {
	return !slices.ContainsFunc(todos, func(t Todo) bool { return t.Status != StatusCompleted })
}

// assistantTurns counts the assistant messages in history and how many of
// them came after the last one that called write_todos (all of them when none
// did).
func assistantTurns(msgs []llm.Message) (turns, sinceWrite int) {
	for _, msg := range msgs {
		if msg.Role != llm.RoleAssistant {
			continue
		}
		turns++
		sinceWrite++
		if callsWriteTodos(msg) {
			sinceWrite = 0
		}
	}
	return turns, sinceWrite
}

func callsWriteTodos(msg llm.Message) bool {
	return slices.ContainsFunc(msg.ToolUses(), func(b llm.Block) bool { return b.Name == ToolWriteTodos })
}
