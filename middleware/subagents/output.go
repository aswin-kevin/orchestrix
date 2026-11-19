package subagents

import (
	"context"
	"fmt"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// structuredOutput checks the answer of a completed subagent run against
// schema and, while it is invalid, continues the run with the validation
// error, up to DefaultOutputRetries times. It returns the last Final and the
// task result: the compact JSON once valid, an error result once the
// retries are spent. The result is nil when a run did not complete (or the
// call was cancelled); that Final is then reported as any other run's.
func structuredOutput(ctx context.Context, child *agent.Agent, in agent.Input, final *agent.Final, schema map[string]any) (*agent.Final, *tool.Result) {
	for retry := 0; ; retry++ {
		if final.Reason != agent.ReasonCompleted || ctx.Err() != nil {
			return final, nil
		}
		answer, err := tool.ParseObject(schema, final.Text)
		if err == nil {
			res := tool.Text(string(answer))
			return final, &res
		}
		if retry == DefaultOutputRetries {
			res := tool.Errorf("subagent %s gave no valid answer after %d attempts: %v", in.AgentName, retry+1, err)
			return final, &res
		}
		in.State = final.State
		in.Messages = []llm.Message{llm.UserText(fmt.Sprintf(invalidOutputPrompt, err))}
		final = runChild(ctx, child, in)
	}
}
