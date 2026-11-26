package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

type editsInput struct {
	Path  string `json:"path"`
	Edits []edit `json:"edits"`
	Mode  string `json:"mode,omitempty" enum:"fast,slow"`
	Count int    `json:"count,omitempty"`
}

func TestValidatePaths(t *testing.T) {
	schema := SchemaFor[editsInput]()
	tests := []struct {
		name, in, wantErr string
	}{
		{"valid", `{"path":"/a","edits":[{"old_string":"x","new_string":"y"}]}`, ""},
		{"nested required", `{"path":"/a","edits":[{"old_string":"x","new_string":"y"},{"new_string":"z"}]}`, "edits[1].old_string: required"},
		{"nested type", `{"path":"/a","edits":[{"old_string":1,"new_string":"y"}]}`, "edits[0].old_string: expected string, got number"},
		{"top required", `{"edits":[]}`, "path: required"},
		{"unknown property", `{"path":"/a","edits":[],"extra":1}`, "extra: unexpected property"},
		{"nested unknown", `{"path":"/a","edits":[{"old_string":"x","new_string":"y","zzz":1}]}`, "edits[0].zzz: unexpected property"},
		{"enum", `{"path":"/a","edits":[],"mode":"medium"}`, "mode: must be one of"},
		{"integer rejects fraction", `{"path":"/a","edits":[],"count":1.5}`, "count: expected integer, got number"},
		{"integer accepts whole", `{"path":"/a","edits":[],"count":3}`, ""},
		{"integer accepts integral decimal", `{"path":"/a","edits":[],"count":1.0}`, ""},
		{"integer accepts integral exponent", `{"path":"/a","edits":[],"count":1e2}`, ""},
		{"integer rejects fractional exponent", `{"path":"/a","edits":[],"count":1.5e0}`, "count: expected integer, got number"},
		{"null optional is absent", `{"path":"/a","edits":[],"mode":null,"count":null}`, ""},
		{"null required is rejected", `{"path":null,"edits":[]}`, "path: expected string, got null"},
		{"null unknown is still unexpected", `{"path":"/a","edits":[],"extra":null}`, "extra: unexpected property"},
		{"not an object", `[1]`, "expected object, got array"},
		{"invalid json", `{`, "invalid JSON"},
		{"trailing data", `{"path":"/a","edits":[]} {}`, "invalid JSON"},
		{"array expected", `{"path":"/a","edits":"no"}`, "edits: expected array, got string"},
		{"null edits", `{"path":"/a","edits":null}`, "edits: expected array, got null"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(schema, json.RawMessage(tt.in))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateAcceptsAnyTypedAndGenericSchemas(t *testing.T) {
	// Schemas decoded from JSON (e.g. supplied by an external tool server)
	// use []any for lists and float64 for numbers.
	var schema map[string]any
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"n":{"type":"number","enum":[1,2]},"b":{"type":"boolean"},"z":{"type":"null"}},"required":["n"],"additionalProperties":false}`), &schema)

	if err := Validate(schema, json.RawMessage(`{"n":2,"b":true,"z":null}`)); err != nil {
		t.Errorf("valid input rejected: %v", err)
	}
	if err := Validate(schema, json.RawMessage(`{"n":3}`)); err == nil {
		t.Error("enum violation accepted")
	}
	if err := Validate(schema, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "n: required") {
		t.Errorf("missing required: %v", err)
	}
}
