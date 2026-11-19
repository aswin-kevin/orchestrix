package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// ParseObject parses text as a JSON object that is valid against schema. The
// text may be wrapped in one code fence and surrounded by whitespace. The
// result is normalized like a typed tool's input (null optional properties
// removed, integral numbers written as integers) and compact, so it decodes
// into a Go value of the schema's type.
func ParseObject(schema map[string]any, text string) (json.RawMessage, error) {
	raw, fenced := StripCodeFence(text)
	if !fenced {
		raw = strings.TrimSpace(text)
	}
	if !strings.HasPrefix(raw, "{") {
		return nil, errors.New("the answer must be a JSON object")
	}
	in := json.RawMessage(raw)
	if err := Validate(schema, in); err != nil {
		return nil, err
	}
	norm, err := normalize(schema, in)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, norm); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
