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
//
// Answers are checked with tool.Validate's subset: type, properties,
// required, enum, items and additionalProperties: false. A hand-written
// schema must use []any for enum lists and map[string]any for nested schemas,
// as tool.SchemaFor produces; other Go types there are not checked. For a Go
// struct, use WithOutputType instead.
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
//	a, err := orchestrix.New("claude-sonnet-4-5", orchestrix.WithOutputType[Verdict]())
//	if err != nil {
//		return err
//	}
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
// the structured output into T. The Final is returned whenever the run
// produced one, also when the run failed, so an interrupted run can be
// resumed from it and a failed one inspected. With a Final the error is
// Decode's, which names the reason and wraps Final.Err (so
// errors.Is(err, agent.ErrInvalidOutput) holds for an invalid_output run);
// without one it is the stream error.
//
//	v, final, err := orchestrix.RunTyped[Verdict](ctx, a, agent.Input{
//		Messages: []llm.Message{llm.UserText("assess this change")},
//	})
func RunTyped[T any](ctx context.Context, a *agent.Agent, in agent.Input) (T, *agent.Final, error) {
	var final *agent.Final
	var runErr error
	for ev, err := range a.Run(ctx, in) {
		// The Final event carries Final.Err as its stream error, so the
		// Final is kept before the error is looked at.
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
		if err != nil && runErr == nil {
			runErr = err
		}
	}
	if final == nil {
		var zero T
		if runErr == nil {
			runErr = errors.New("orchestrix: no final event")
		}
		return zero, nil, runErr
	}
	v, err := Decode[T](final)
	if err == nil && runErr != nil {
		var zero T
		return zero, final, runErr
	}
	return v, final, err
}
