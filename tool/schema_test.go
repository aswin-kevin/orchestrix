package tool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type edit struct {
	OldString string `json:"old_string" desc:"text to replace"`
	NewString string `json:"new_string"`
}

type base struct {
	ID string `json:"id"`
}

type tagged struct {
	base
	Path    string          `json:"path" desc:"file path"`
	Limit   *int            `json:"limit"`
	Note    string          `json:"note,omitempty"`
	Mode    string          `json:"mode" enum:"fast,slow"`
	Edits   []edit          `json:"edits"`
	Labels  map[string]bool `json:"labels,omitempty"`
	Any     any             `json:"any,omitempty"`
	Raw     json.RawMessage `json:"raw,omitempty"`
	Ratio   float64         `json:"ratio,omitempty"`
	Skipped string          `json:"-"`
	hidden  string
}

func TestSchemaForTags(t *testing.T) {
	_ = tagged{}.hidden
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":    map[string]any{"type": "string"},
			"path":  map[string]any{"type": "string", "description": "file path"},
			"limit": map[string]any{"type": "integer"},
			"note":  map[string]any{"type": "string"},
			"mode":  map[string]any{"type": "string", "enum": []any{"fast", "slow"}},
			"edits": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old_string": map[string]any{"type": "string", "description": "text to replace"},
						"new_string": map[string]any{"type": "string"},
					},
					"required":             []string{"old_string", "new_string"},
					"additionalProperties": false,
				},
			},
			"labels": map[string]any{"type": "object"},
			"any":    map[string]any{},
			"raw":    map[string]any{},
			"ratio":  map[string]any{"type": "number"},
		},
		"required":             []string{"id", "path", "mode", "edits"},
		"additionalProperties": false,
	}
	if got := SchemaFor[tagged](); !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", " ")
		wantJSON, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("schema mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestSchemaForUnsupportedKindsPanicWithFieldPath(t *testing.T) {
	type inner struct {
		C chan int `json:"c"`
	}
	type outer struct {
		Items []inner `json:"items"`
	}
	defer func() {
		r := recover()
		msg, _ := r.(string)
		if msg == "" || !strings.Contains(msg, "items[].c") {
			t.Errorf("panic = %v, want a message naming items[].c", r)
		}
	}()
	SchemaFor[outer]()
}

func TestSchemaForRejectsNonStruct(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("SchemaFor[int] should panic")
		}
	}()
	SchemaFor[int]()
}
