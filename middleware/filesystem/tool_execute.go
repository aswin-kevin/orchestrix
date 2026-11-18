package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/tool"
)

type executeInput struct {
	Command        string `json:"command" desc:"Shell command to run."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" desc:"Seconds to wait before the command is stopped. Defaults to the configured timeout and is capped at the configured maximum."`
}

// registersExecute reports whether the execute tool is offered; see
// Config.Execute for why a nil Backend needs the explicit opt-in.
func (m *middleware) registersExecute() bool {
	if m.cfg.Backend == nil {
		return m.cfg.Execute
	}
	_, ok := m.cfg.Backend.(backend.Executor)
	return ok
}

// executeTool has no permission function of its own, so it falls through to
// the default "ask" verdict: running commands is never pre-approved here.
func (m *middleware) executeTool() tool.Tool {
	return tool.New(ToolExecute,
		m.description(ToolExecute, "Run a shell command in the workspace and return its combined output followed by the exit code. Commands that exceed the timeout are stopped."),
		m.execute,
		tool.WithSubject(func(raw json.RawMessage) string {
			var in executeInput
			_ = json.Unmarshal(raw, &in) // an unparsable input has no subject
			return in.Command
		}),
	)
}

func (m *middleware) execute(ctx context.Context, in executeInput, c *tool.Call) (tool.Result, error) {
	b, err := m.backendFor(c)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	exec, ok := b.(backend.Executor)
	if !ok {
		return tool.Errorf("this backend cannot execute commands"), nil
	}
	timeout := m.execTimeout(in.TimeoutSeconds)
	res, err := exec.Execute(ctx, in.Command, backend.ExecOptions{Timeout: timeout})
	if err != nil {
		return tool.Errorf("execute failed: %v", err), nil
	}
	out := tool.Text(formatExec(res, timeout))
	out.IsError = res.TimedOut
	return out, nil
}

// execTimeout applies the default and the cap to the model's request.
func (m *middleware) execTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return min(m.cfg.ExecTimeout, m.cfg.MaxExecTimeout)
	}
	return min(time.Duration(seconds)*time.Second, m.cfg.MaxExecTimeout)
}

func formatExec(res backend.ExecResult, timeout time.Duration) string {
	var sb strings.Builder
	sb.WriteString(res.Output)
	if res.Truncated {
		sb.WriteString("\n[output truncated]")
	}
	if res.TimedOut {
		fmt.Fprintf(&sb, "\n[timed out after %s]", timeout)
	}
	fmt.Fprintf(&sb, "\n[exit code %d]", res.ExitCode)
	return sb.String()
}
