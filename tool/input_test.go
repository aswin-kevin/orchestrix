package tool

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCheckInput(t *testing.T) {
	strict := Build(Spec{
		Name:   "strict",
		Schema: map[string]any{"type": "object", "required": []any{"path"}, "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		Run:    func(context.Context, json.RawMessage, *Call) (Result, error) { return Text("ok"), nil },
	})
	raw := Build(Spec{
		Name:     "raw",
		Schema:   map[string]any{"type": "object", "required": []any{"path"}},
		RawInput: true,
		Run:      func(context.Context, json.RawMessage, *Call) (Result, error) { return Text("ok"), nil },
	})
	if err := CheckInput(strict, json.RawMessage(`{"path":"/a"}`)); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if err := CheckInput(strict, json.RawMessage(`{}`)); err == nil {
		t.Fatal("input missing a required field accepted")
	}
	if err := CheckInput(raw, json.RawMessage(`not json`)); err != nil {
		t.Fatalf("raw-input tool was validated: %v", err)
	}
}

func TestStripCodeFence(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"```json\n{\"x\":1}\n```", `{"x":1}`, true},
		{"  ```\n{\"x\":1}\n```  ", `{"x":1}`, true},
		{"```go\nfmt.Println()\n```", "fmt.Println()", true},
		{`{"x":1}`, "", false},
		{"```{\"x\":1}```", "", false}, // the opening fence must end its line
		{"```", "", false},
	}
	for _, tt := range tests {
		if got, ok := StripCodeFence(tt.in); got != tt.want || ok != tt.ok {
			t.Errorf("StripCodeFence(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
