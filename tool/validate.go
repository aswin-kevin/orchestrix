package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Validate checks the JSON document in against a JSON schema. It supports the
// subset tools need: type (object, string, integer, number, boolean, array,
// null), properties, required, enum, items and additionalProperties:false.
// Errors name the offending location, for example "edits[1].old_string: required".
func Validate(schema map[string]any, in json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.UseNumber() // keeps 1.5 distinguishable from 1 for integer checks
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON: unexpected data after the top-level value")
	}
	return validateValue(schema, v, "")
}

func validateValue(schema map[string]any, v any, path string) error {
	if typ, ok := schema["type"].(string); ok {
		if err := checkType(typ, v, path); err != nil {
			return err
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		if err := checkEnum(enum, v, path); err != nil {
			return err
		}
	}
	switch val := v.(type) {
	case map[string]any:
		return validateObject(schema, val, path)
	case []any:
		return validateArray(schema, val, path)
	}
	return nil
}

func validateObject(schema map[string]any, obj map[string]any, path string) error {
	for _, name := range requiredNames(schema) {
		if _, ok := obj[name]; !ok {
			return pathError(joinPath(path, name), "required")
		}
	}
	props, _ := schema["properties"].(map[string]any)
	for _, key := range sortedKeys(obj) {
		propSchema, known := props[key].(map[string]any)
		if !known {
			if schema["additionalProperties"] == false {
				return pathError(joinPath(path, key), "unexpected property")
			}
			continue
		}
		if err := validateValue(propSchema, obj[key], joinPath(path, key)); err != nil {
			return err
		}
	}
	return nil
}

func validateArray(schema map[string]any, items []any, path string) error {
	itemSchema, ok := schema["items"].(map[string]any)
	if !ok {
		return nil
	}
	for i, item := range items {
		if err := validateValue(itemSchema, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

// requiredNames accepts both []string (generated schemas) and []any (schemas
// decoded from JSON).
func requiredNames(schema map[string]any) []string {
	switch r := schema["required"].(type) {
	case []string:
		return r
	case []any:
		names := make([]string, 0, len(r))
		for _, n := range r {
			if s, ok := n.(string); ok {
				names = append(names, s)
			}
		}
		return names
	}
	return nil
}

func checkType(typ string, v any, path string) error {
	if matchesType(typ, v) {
		return nil
	}
	return pathError(path, fmt.Sprintf("expected %s, got %s", typ, jsonTypeName(v)))
}

func matchesType(typ string, v any) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, err := n.Int64()
		return err == nil
	}
	return true // unknown type keywords are not enforced
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		return "number"
	case []any:
		return "array"
	default:
		return "object"
	}
}

// checkEnum compares by printed form so that a number decoded from the input
// (json.Number) equals the same number in a schema decoded from JSON (float64).
func checkEnum(enum []any, v any, path string) error {
	got := fmt.Sprint(v)
	for _, allowed := range enum {
		if fmt.Sprint(allowed) == got {
			return nil
		}
	}
	return pathError(path, fmt.Sprintf("must be one of %v", enum))
}

// pathError prefixes msg with its location; the document root has no path.
func pathError(path, msg string) error {
	if path == "" {
		return errors.New(msg)
	}
	return fmt.Errorf("%s: %s", path, msg)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
