package tool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
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

type level int

func (level) MarshalText() ([]byte, error) { return []byte("x"), nil }

type valueHook string

func (valueHook) JSONSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": "^x"}
}

type ptrHook string

func (*ptrHook) JSONSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": "^p"}
}

type hookArgs struct {
	V valueHook `json:"v"`
	P ptrHook   `json:"p"`
}

func TestSchemaForTime(t *testing.T) {
	type args struct {
		At time.Time `json:"at"`
	}
	got := SchemaFor[args]()["properties"].(map[string]any)["at"]
	want := map[string]any{"type": "string", "format": "date-time"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("time schema = %v, want %v", got, want)
	}
}

func TestSchemaForBytes(t *testing.T) {
	type args struct {
		Data []byte `json:"data"`
	}
	got := SchemaFor[args]()["properties"].(map[string]any)["data"]
	want := map[string]any{"type": "string", "contentEncoding": "base64"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bytes schema = %v, want %v", got, want)
	}
}

func TestSchemaForTextMarshaler(t *testing.T) {
	type args struct {
		L level `json:"l" enum:"low,high"`
		M level `json:"m"`
	}
	props := SchemaFor[args]()["properties"].(map[string]any)
	if want := map[string]any{"type": "string", "enum": []any{"low", "high"}}; !reflect.DeepEqual(props["l"], want) {
		t.Fatalf("enum text schema = %v, want %v", props["l"], want)
	}
	if want := map[string]any{"type": "string"}; !reflect.DeepEqual(props["m"], want) {
		t.Fatalf("text schema = %v, want %v", props["m"], want)
	}
}

func TestSchemaForJSONSchemaHook(t *testing.T) {
	props := SchemaFor[hookArgs]()["properties"].(map[string]any)
	if want := map[string]any{"type": "string", "pattern": "^x"}; !reflect.DeepEqual(props["v"], want) {
		t.Fatalf("value hook = %v, want %v", props["v"], want)
	}
	if want := map[string]any{"type": "string", "pattern": "^p"}; !reflect.DeepEqual(props["p"], want) {
		t.Fatalf("pointer hook = %v, want %v", props["p"], want)
	}
}

func TestSchemaForHookOnTopLevelIsCopied(t *testing.T) {
	got := SchemaFor[topHook]()
	got["extra"] = true
	if _, ok := SchemaFor[topHook]()["extra"]; ok {
		t.Fatal("hook result is shared between calls")
	}
	if got["type"] != "object" {
		t.Fatalf("top-level hook ignored: %v", got)
	}
}

type topHook struct{ A int }

var topHookSchema = map[string]any{"type": "object"}

func (topHook) JSONSchema() map[string]any { return topHookSchema }

func TestSchemaForRawMessageUnchanged(t *testing.T) {
	type args struct {
		Raw json.RawMessage `json:"raw"`
	}
	got := SchemaFor[args]()["properties"].(map[string]any)["raw"]
	if want := map[string]any{}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RawMessage schema = %v, want %v", got, want)
	}
}

type nilHook string

func (nilHook) JSONSchema() map[string]any { return nil }

func TestSchemaForNilHookIsEmptySchema(t *testing.T) {
	type args struct {
		N nilHook `json:"n"`
		D nilHook `json:"d" desc:"described"`
	}
	props := SchemaFor[args]()["properties"].(map[string]any)
	if got, ok := props["n"].(map[string]any); !ok || got == nil || len(got) != 0 {
		t.Fatalf("nil hook schema = %#v, want a non-nil empty map", props["n"])
	}
	if want := map[string]any{"description": "described"}; !reflect.DeepEqual(props["d"], want) {
		t.Fatalf("described nil hook schema = %#v, want %v", props["d"], want)
	}
}

func TestSchemaForTopLevelHookMustBeObject(t *testing.T) {
	for name, f := range map[string]func(){
		"string": func() { SchemaFor[valueHook]() },
		"nil":    func() { SchemaFor[nilHook]() },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				msg, _ := recover().(string)
				if !strings.Contains(msg, "tool.SchemaFor:") {
					t.Errorf("panic = %q, want a tool.SchemaFor panic", msg)
				}
			}()
			f()
		})
	}
}
