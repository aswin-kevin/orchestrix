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

// backgroundExecuteInput is the input of execute when Config.Background is
// set; a separate type keeps run_in_background out of the schema otherwise.
type backgroundExecuteInput struct {
	executeInput
	RunInBackground bool `json:"run_in_background,omitempty" desc:"Start the command and return immediately; you will be notified when it finishes."`
}

// registersExecute reports whether the execute tool is offered; see
// Config.Execute for why a nil Backend needs the explicit opt-in.
func (m *middleware) registersExecute() bool {
	if m.cfg.Backend == nil {
		return m.cfg.Execute
	}
	_, ok := backend.AsExecutor(m.cfg.Backend)
	return ok
}

// executeTool has no permission function of its own, so it falls through to
// the default "ask" verdict: running commands is never pre-approved here.
func (m *middleware) executeTool() tool.Tool {
	description := m.description(ToolExecute, "Run a shell command in the workspace and return its combined output followed by the exit code. Each command runs in a fresh shell from the workspace root, so `cd` and `export` do not carry over; chain steps with `&&`. Commands that exceed the timeout are stopped.")
	subject := tool.WithShellSubject(func(raw json.RawMessage) string {
		var in executeInput
		_ = json.Unmarshal(raw, &in) // an unparsable input has no subject
		return in.Command
	})
	if m.cfg.Background {
		return tool.New(ToolExecute, description, m.execute, subject)
	}
	foreground := func(ctx context.Context, in executeInput, c *tool.Call) (tool.Result, error) {
		return m.execute(ctx, backgroundExecuteInput{executeInput: in}, c)
	}
	return tool.New(ToolExecute, description, foreground, subject)
}

func (m *middleware) execute(ctx context.Context, in backgroundExecuteInput, c *tool.Call) (tool.Result, error) {
	if in.RunInBackground {
		return m.executeInBackground(ctx, in.executeInput, c), nil
	}
	b, err := m.backendFor(c)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	exec, ok := backend.AsExecutor(b)
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

// executeInBackground hands the command to the run's background starter,
// with the backend the foreground command would use. The timeout is passed
// as asked, zero when none was: the starter applies its own default and cap,
// which suit long-running jobs better than the foreground ones.
func (m *middleware) executeInBackground(ctx context.Context, in executeInput, c *tool.Call) tool.Result {
	if c == nil || c.Background == nil {
		return tool.Errorf("background execution is not available")
	}
	b, err := m.backendFor(c)
	if err != nil {
		return tool.Errorf("%v", err)
	}
	withBackend := *c
	withBackend.Backend = b
	timeout := time.Duration(max(in.TimeoutSeconds, 0)) * time.Second
	id, err := c.Background.Start(ctx, &withBackend, in.Command, timeout)
	if err != nil {
		return tool.Errorf("background start failed: %v", err)
	}
	return tool.Text(fmt.Sprintf("Started background task %s. Use task_output to read its output; you will be notified when it finishes.", id))
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
