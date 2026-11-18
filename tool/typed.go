package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

// New builds a tool whose input is the struct In. The schema is generated from
// In (see SchemaFor), and input is validated against it and then decoded
// strictly before fn runs, so fn only ever sees well-formed input. Invalid
// input is reported to the model as an error Result, not as a Go error.
// New panics on an invalid name or an In that has no schema.
func New[In any](name, description string, fn func(ctx context.Context, in In, c *Call) (Result, error), opts ...Option) Tool {
	schema := SchemaFor[In]()
	spec := Spec{
		Name:        name,
		Description: description,
		Schema:      schema,
		Run: func(ctx context.Context, raw json.RawMessage, c *Call) (Result, error) {
			in, err := decodeInput[In](schema, raw)
			if err != nil {
				return Errorf("invalid input: %v", err), nil
			}
			return fn(ctx, in, c)
		},
	}
	for _, opt := range opts {
		opt(&spec)
	}
	return Build(spec)
}

// decodeInput validates raw against schema, then decodes it into In rejecting
// unknown fields. Models sometimes send nothing for parameterless tools, which
// is treated as an empty object.
func decodeInput[In any](schema map[string]any, raw json.RawMessage) (In, error) {
	var in In
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := Validate(schema, raw); err != nil {
		return in, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, err
	}
	return in, nil
}
