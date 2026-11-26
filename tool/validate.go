package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
)

// Validate checks the JSON document in against a JSON schema. It supports the
// subset tools need: type (object, string, integer, number, boolean, array,
// null), properties, required, enum, items and additionalProperties:false.
// Errors name the offending location, for example "edits[1].old_string:
// required".
//
// Two leniencies match what models commonly send: null for a property that is
// not required counts as leaving it out, and a number with an integral value
// written as a decimal or with an exponent (1.0, 1e2) is an integer.
func Validate(schema map[string]any, in json.RawMessage) error {
	v, err := decodeJSON(in)
	if err != nil {
		return err
	}
	return validateValue(schema, v, "")
}

// decodeJSON decodes a single JSON document, keeping numbers as json.Number
// so that 1.5 stays distinguishable from 1 for integer checks.
func decodeJSON(in json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid JSON: unexpected data after the top-level value")
	}
	return v, nil
}

// normalize rewrites a document that passed Validate into the form Go's
// decoder accepts for the same schema: null optional properties are removed
// and integral numbers in integer positions are written as plain integers.
func normalize(schema map[string]any, in json.RawMessage) (json.RawMessage, error) {
	v, err := decodeJSON(in)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalizeValue(schema, v))
}

func normalizeValue(schema map[string]any, v any) any {
	switch val := v.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for key, item := range val {
			propSchema, known := props[key].(map[string]any)
			switch {
			case known && isAbsent(schema, key, item):
				delete(val, key)
			case known:
				val[key] = normalizeValue(propSchema, item)
			}
		}
	case []any:
		itemSchema, _ := schema["items"].(map[string]any)
		for i, item := range val {
			val[i] = normalizeValue(itemSchema, item)
		}
	case json.Number:
		if schema["type"] == "integer" {
			return integerNumber(val)
		}
	}
	return v
}

// isAbsent reports whether the property key of an object counts as left out:
// it is null and not required.
func isAbsent(schema map[string]any, key string, v any) bool {
	return v == nil && !slices.Contains(requiredNames(schema), key)
}

// integerNumber writes an integral number such as 1e2 as plain digits (100).
// Anything else is returned unchanged.
func integerNumber(n json.Number) json.Number {
	if _, err := n.Int64(); err == nil {
		return n
	}
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) {
		return n
	}
	return json.Number(strconv.FormatFloat(f, 'f', -1, 64))
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
		if isAbsent(schema, key, obj[key]) {
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
		return ok && isIntegral(n)
	}
	return true // unknown type keywords are not enforced
}

// isIntegral reports whether n has no fractional part, however it is
// written: 3, 3.0 and 3e0 all qualify.
func isIntegral(n json.Number) bool {
	if _, err := n.Int64(); err == nil {
		return true
	}
	f, err := n.Float64()
	return err == nil && f == math.Trunc(f)
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
// (json.Number) equals the same number in a schema decoded from JSON
// (float64).
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
