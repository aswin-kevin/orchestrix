package subagents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
		answer, err := parseAnswer(schema, final.Text)
		if err == nil {
			res := tool.Text(answer)
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

// parseAnswer validates text, with any surrounding code fence removed, as a
// JSON object against schema and returns it compacted.
func parseAnswer(schema map[string]any, text string) (string, error) {
	raw, fenced := tool.StripCodeFence(text)
	if !fenced {
		raw = strings.TrimSpace(text)
	}
	if !strings.HasPrefix(raw, "{") {
		return "", errors.New("the answer must be a JSON object")
	}
	if err := tool.Validate(schema, json.RawMessage(raw)); err != nil {
		return "", err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(raw)); err != nil {
		return "", err
	}
	return compact.String(), nil
}
