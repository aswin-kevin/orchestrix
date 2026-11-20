package subagents

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// TaskInput is the input of the task tool.
type TaskInput struct {
	Description  string `json:"description" desc:"The complete task, with every detail the subagent needs; only a fork sees this conversation."`
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
		// Starting a subagent changes nothing by itself: each tool the
		// subagent calls goes through the subagent's own permission checks.
		// Being read-only, the task tool is allowed in plan and dontAsk
		// modes too (see the package documentation).
		ReadOnly: func(json.RawMessage) bool { return true },
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

// describe is the description of subagent_type name.
func (m *middleware) describe(name string) string {
	if name == Fork {
		return forkDescription
	}
	return m.specs[name].Description
}

// describeTask lists every subagent with its description after the tool's
// own description.
func (m *middleware) describeTask(order []string) string {
	var b strings.Builder
	b.WriteString(taskDescription)
	for _, name := range order {
		fmt.Fprintf(&b, "\n- %s: %s", name, m.describe(name))
	}
	return b.String()
}

// childRun is one subagent run of a task call.
type childRun struct {
	// typ is the subagent_type, which is also the name the run reports.
	typ   string
	agent *agent.Agent
	// in is the run's input; a continuation replaces its State and Messages.
	in agent.Input
	// schema is the subagent's OutputSchema; nil for free text.
	schema map[string]any
}

// runTask runs one task call: it starts the selected subagent, or continues
// the one suspended under a resumed call, forwards its events, merges its
// state back and turns its outcome into the result.
func (m *middleware) runTask(ctx context.Context, raw json.RawMessage, c *tool.Call) (tool.Result, error) {
	var in TaskInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tool.Errorf("invalid input: %v", err), nil
	}
	if forked, _ := forkedKey.Get(c.State); forked {
		return tool.Errorf(forkCannotDelegate), nil
	}
	if c.Resume != nil {
		return m.resumeTask(ctx, c)
	}
	if !m.offers(in.SubagentType) {
		return tool.Errorf("unknown subagent type %q", in.SubagentType), nil
	}
	info, ok := agent.Info(ctx)
	if !ok {
		return tool.Errorf("the task tool can only run inside an agent run"), nil
	}
	if info.Depth >= m.maxDepth {
		return tool.Errorf("cannot start subagent %s: the maximum nesting depth of %d is reached; do the task yourself", in.SubagentType, m.maxDepth), nil
	}
	run, err := m.start(ctx, info, in, c)
	if err != nil {
		return tool.Errorf("cannot start subagent %s: %v", in.SubagentType, err), nil
	}
	final := runChild(ctx, run.agent, run.in)
	return m.settle(ctx, c, run, final)
}

// offers reports whether typ is a subagent the task tool can start.
func (m *middleware) offers(typ string) bool {
	if typ == Fork {
		return m.fork
	}
	_, ok := m.specs[typ]
	return ok
}

// start prepares a fresh run of the subagent in selects.
func (m *middleware) start(ctx context.Context, info agent.RunInfo, in TaskInput, c *tool.Call) (childRun, error) {
	child, schema, err := m.subagent(info.Agent, in.SubagentType)
	if err != nil {
		return childRun{}, err
	}
	run := childRun{typ: in.SubagentType, agent: child, schema: schema}
	if in.SubagentType == Fork {
		run.in, err = forkInput(info.ThreadID, in.Description, c)
	} else {
		run.in, err = m.launcher.Input(ctx, child, m.specs[in.SubagentType], in.Description, c.ToolUseID, c)
	}
	return run, err
}

// subagent returns the agent that runs subagent_type typ when started from
// parent, with its OutputSchema. A fork is the parent itself.
func (m *middleware) subagent(parent *agent.Agent, typ string) (*agent.Agent, map[string]any, error) {
	if !m.offers(typ) {
		return nil, nil, fmt.Errorf("unknown subagent type %q", typ)
	}
	if typ == Fork {
		return parent, nil, nil
	}
	spec := m.specs[typ]
	child, err := m.launcher.Agent(parent, spec)
	return child, spec.OutputSchema, err
}

// settle turns a child's Final into the task result: it checks a structured
// answer, suspends a child waiting for a decision, and otherwise merges the
// child's state back. A suspended child's state is merged once it finishes.
// A child whose stream ended without a Final (nil) gives an error result, or
// the context's error when the call was cancelled.
func (m *middleware) settle(ctx context.Context, c *tool.Call, run childRun, final *agent.Final) (tool.Result, error) {
	var structured *tool.Result
	if run.schema != nil {
		final, structured = structuredOutput(ctx, run.agent, run.in, final, run.schema)
	}
	if final == nil {
		if err := ctx.Err(); err != nil {
			return tool.Result{}, err
		}
		return tool.Errorf("subagent %s ended without a result", run.typ), nil
	}
	if final.Reason == agent.ReasonInterrupted && ctx.Err() == nil {
		return suspend(c, run, final)
	}
	if final.State != nil {
		c.State.MergeChild(final.State)
	}
	if structured != nil {
		return *structured, nil
	}
	return outcome(ctx, run.typ, final)
}

// runChild runs child to its end and returns its Final (see drain).
func runChild(ctx context.Context, child *agent.Agent, in agent.Input) *agent.Final {
	return drain(ctx, child.Run(ctx, in))
}

// drain consumes a child's event stream, forwarding its events to the
// parent's stream, and returns the Final (nil when the stream ended without
// one). The Final becomes the task result, and a KindInterrupt is not
// forwarded either: the parent raises its own interrupt for the task call, so
// consumers see exactly one per call. drain
// always consumes the whole stream, so the child's goroutines have stopped
// when it returns; cancelling ctx (the parent's call context) stops the
// child.
func drain(ctx context.Context, events iter.Seq2[agent.Event, error]) *agent.Final {
	var final *agent.Final
	for ev := range events {
		switch ev.Kind {
		case agent.KindFinal:
			final = ev.Final
		case agent.KindInterrupt:
			// Raised again by the parent for the task call (see suspend).
		default:
			agent.Forward(ctx, ev)
		}
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
	case agent.ReasonMaxTurns, agent.ReasonTokenLimit, agent.ReasonTimeout:
		return tool.Text(fmt.Sprintf("%s\n[subagent stopped: %s]", finalText(final), final.Reason)), nil
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
