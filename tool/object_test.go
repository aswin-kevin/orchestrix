package tool

import (
	"strings"
	"testing"
)

var answerSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"n": map[string]any{"type": "integer"},
		"s": map[string]any{"type": "string"},
	},
	"required":             []any{"n"},
	"additionalProperties": false,
}

func TestParseObjectPlain(t *testing.T) {
	got, err := ParseObject(answerSchema, " {\"n\": 1, \"s\": \"a\"} \n")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"n":1,"s":"a"}` {
		t.Errorf("got %s", got)
	}
}

func TestParseObjectFenced(t *testing.T) {
	got, err := ParseObject(answerSchema, "```json\n{\"n\":1}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"n":1}` {
		t.Errorf("got %s", got)
	}
}

func TestParseObjectNormalizes(t *testing.T) {
	got, err := ParseObject(answerSchema, `{"n":1e2,"s":null}`)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"n":100}` {
		t.Errorf("got %s", got)
	}
}

func TestParseObjectRejects(t *testing.T) {
	tests := []struct{ in, want string }{
		{`hello`, "must be a JSON object"},
		{`[1]`, "must be a JSON object"},
		{`{"s":"a"}`, "n"},
		{`{"n":1,"x":2}`, "x"},
		{`{"n":1.5}`, "n"},
	}
	for _, tt := range tests {
		_, err := ParseObject(answerSchema, tt.in)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ParseObject(%q) error = %v, want containing %q", tt.in, err, tt.want)
		}
	}
}
