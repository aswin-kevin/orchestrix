package subagents

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// TaskInput is the input of the task tool.
type TaskInput struct {
	Description  string `json:"description" desc:"The complete task, with every detail the subagent needs; it does not see this conversation."`
	SubagentType string `json:"subagent_type" desc:"Which subagent to use."`
}

// taskTool builds the task tool offering the subagents named in order.
func (m *middleware) taskTool(order []string) tool.Tool {
	return tool.Build(tool.Spec{
		Name:        ToolTask,
		Description: m.describeTask(order),
		Schema:      taskSchema(order),
		Run:         m.runTask,
		// Parallel task calls run together.
		Concurrent: func(json.RawMessage) bool { return true },
		// Starting a subagent is harmless in itself: each tool the subagent
		// calls goes through the subagent's own permission checks.
		Permission: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
	})
}

// taskSchema is TaskInput's schema with subagent_type limited to the
// configured names, which are only known at New.
func taskSchema(order []string) map[string]any {
	schema := tool.SchemaFor[TaskInput]()
	props := maps.Clone(schema["properties"].(map[string]any))
	typ := maps.Clone(props["subagent_type"].(map[string]any))
	enum := make([]any, len(order)) // tool.Validate reads enum as []any
	for i, name := range order {
		enum[i] = name
	}
	typ["enum"] = enum
	props["subagent_type"] = typ
	schema["properties"] = props
	return schema
}

// describeTask lists every subagent with its description after the tool's
// own description.
func (m *middleware) describeTask(order []string) string {
	var b strings.Builder
	b.WriteString(taskDescription)
	for _, name := range order {
		fmt.Fprintf(&b, "\n- %s: %s", name, m.specs[name].Description)
	}
	return b.String()
}

// runTask runs one task call: it starts the selected subagent on the
// description alone, forwards its events, merges its state back and turns
// its outcome into the result.
func (m *middleware) runTask(ctx context.Context, raw json.RawMessage, c *tool.Call) (tool.Result, error) {
	var in TaskInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tool.Errorf("invalid input: %v", err), nil
	}
	spec, ok := m.specs[in.SubagentType]
	if !ok {
		return tool.Errorf("unknown subagent type %q", in.SubagentType), nil
	}
	info, ok := agent.Info(ctx)
	if !ok {
		return tool.Errorf("the task tool can only run inside an agent run"), nil
	}
	if info.Depth >= m.maxDepth {
		return tool.Errorf("cannot start subagent %s: the maximum nesting depth of %d is reached; do the task yourself", spec.Name, m.maxDepth), nil
	}
	child, err := m.child(info.Agent, spec)
	if err != nil {
		return tool.Errorf("cannot start subagent %s: %v", spec.Name, err), nil
	}
	childIn := childInput(info.ThreadID, spec.Name, in.Description, c)
	final := runChild(ctx, child, childIn)
	var structured *tool.Result
	if spec.OutputSchema != nil {
		final, structured = structuredOutput(ctx, child, childIn, final, spec.OutputSchema)
	}
	if final.State != nil {
		c.State.MergeChild(final.State)
	}
	if structured != nil {
		return *structured, nil
	}
	return outcome(ctx, spec.Name, final)
}

// childInput starts a subagent run on the task description alone, with the
// parent's shareable state and backend, under a thread id unique to the call.
func childInput(parentThread, name, description string, c *tool.Call) agent.Input {
	return agent.Input{
		ThreadID:  parentThread + "/" + name + "/" + c.ToolUseID,
		State:     c.State.ChildFor(),
		Messages:  []llm.Message{llm.UserText(description)},
		Backend:   c.Backend,
		AgentName: name,
		// The parent's thread is the one that is saved.
		Ephemeral: true,
	}
}

// runChild runs child to its end, forwarding every event but the Final to
// the parent's stream, and returns the Final. It always drains the stream,
// so the child's goroutines have stopped when it returns; cancelling ctx
// (the parent's call context) stops the child.
func runChild(ctx context.Context, child *agent.Agent, in agent.Input) *agent.Final {
	var final *agent.Final
	for ev := range child.Run(ctx, in) {
		if ev.Kind == agent.KindFinal {
			final = ev.Final
			continue
		}
		agent.Forward(ctx, ev)
	}
	return final
}

// outcome turns a finished subagent run into the task result. A cancelled
// call returns the context's error, so the parent reports the call as
// cancelled rather than as a subagent failure.
func outcome(ctx context.Context, name string, final *agent.Final) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	switch final.Reason {
	case agent.ReasonCompleted:
		return tool.Text(finalText(final)), nil
	case agent.ReasonMaxTurns, agent.ReasonTimeout:
		return tool.Text(fmt.Sprintf("%s\n[subagent stopped: %s]", finalText(final), final.Reason)), nil
	case agent.ReasonInterrupted:
		return tool.Errorf("subagent needs approval"), nil
	default:
		return tool.Errorf("subagent %s failed (%s): %v", name, final.Reason, final.Err), nil
	}
}

// finalText is the subagent's last reply, or a note that it gave none.
func finalText(final *agent.Final) string {
	if final.Text == "" {
		return noTextResult
	}
	return final.Text
}
