package orchestrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/tool"
)

// WithOutputSchema makes the agent's final answer a JSON object valid against
// schema. The agent asks for that format, checks the reply, and asks again
// (up to agent.OutputRetries times) when it does not match. A completed run
// carries the normalized answer in Final.Output. New fails when the schema's
// top-level "type" is not "object".
func WithOutputSchema(schema map[string]any) Option {
	return func(s *settings) { s.cfg.OutputSchema = schema }
}

// WithOutputType is WithOutputSchema with the schema generated from the
// struct type T by tool.SchemaFor (json, desc and enum tags; omitempty and
// pointer fields are optional). It panics when T is not a struct, as
// tool.SchemaFor does.
//
//	type Verdict struct {
//		Severity string `json:"severity" enum:"low,high"`
//	}
//	a, _ := orchestrix.New("claude-sonnet-4-5", orchestrix.WithOutputType[Verdict]())
//	v, _, err := orchestrix.RunTyped[Verdict](ctx, a, in)
func WithOutputType[T any]() Option {
	return WithOutputSchema(tool.SchemaFor[T]())
}

// Decode strictly decodes the structured output of a finished run into T;
// unknown fields are refused. It fails for a nil final, for any reason other
// than agent.ReasonCompleted (the error names the reason and wraps
// Final.Err, so errors.Is(err, agent.ErrInvalidOutput) works), and when the
// run has no structured output.
func Decode[T any](final *agent.Final) (T, error) {
	var out T
	if final == nil {
		return out, errors.New("orchestrix: no final event")
	}
	if final.Reason != agent.ReasonCompleted {
		if final.Err != nil {
			return out, fmt.Errorf("orchestrix: run ended with %s: %w", final.Reason, final.Err)
		}
		return out, fmt.Errorf("orchestrix: run ended with %s", final.Reason)
	}
	if final.Output == nil {
		return out, errors.New("orchestrix: the run has no structured output (no OutputSchema?)")
	}
	dec := json.NewDecoder(bytes.NewReader(final.Output))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		var zero T
		return zero, fmt.Errorf("orchestrix: decode structured output: %w", err)
	}
	return out, nil
}

// RunTyped runs a to completion, ignoring intermediate events, and decodes
// the structured output into T. It returns the first stream error, if any,
// and otherwise the result of Decode. The Final is returned whenever the run
// produced one, so an interrupted run can be resumed from it.
//
//	v, final, err := orchestrix.RunTyped[Verdict](ctx, a, agent.Input{
//		Messages: []llm.Message{llm.UserText("assess this change")},
//	})
func RunTyped[T any](ctx context.Context, a *agent.Agent, in agent.Input) (T, *agent.Final, error) {
	var final *agent.Final
	for ev, err := range a.Run(ctx, in) {
		if err != nil {
			var zero T
			return zero, final, err
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	v, err := Decode[T](final)
	return v, final, err
}
