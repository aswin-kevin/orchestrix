package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// answerSchema is the schema of the structured-output tests.
var answerSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"n": map[string]any{"type": "integer"},
		"s": map[string]any{"type": "string"},
	},
	"required":             []any{"n"},
	"additionalProperties": false,
}

// compactSchema is answerSchema as the prompt segment carries it.
func compactSchema(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(answerSchema)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewRejectsBadOutputSchema(t *testing.T) {
	p := llmtest.New()
	cases := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{"array", map[string]any{"type": "array"}, "object"},
		{"no type", map[string]any{"properties": map[string]any{}}, "object"},
		{"unmarshalable", map[string]any{"type": "object", "x": func() {}}, "OutputSchema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{Provider: p, OutputSchema: tc.schema})
			if err == nil {
				t.Fatal("New succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestOutputPromptSegment(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: `{"n":1}`})
	a := mustNew(t, Config{Provider: p, System: []string{"base"}, CacheBoundary: 1, OutputSchema: answerSchema})
	collect(t, a.Run(context.Background(), userInput("go")))
	req := p.Requests[0]
	want := []string{"base", OutputFormatPrompt + compactSchema(t)}
	if !slices.Equal(req.System, want) {
		t.Errorf("System = %q, want %q", req.System, want)
	}
	if req.CacheBoundary != 2 {
		t.Errorf("CacheBoundary = %d, want 2", req.CacheBoundary)
	}
}

func TestOutputPromptKeepsShorterCacheBoundary(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: `{"n":1}`})
	a := mustNew(t, Config{Provider: p, System: []string{"a", "b"}, CacheBoundary: 1, OutputSchema: answerSchema})
	collect(t, a.Run(context.Background(), userInput("go")))
	req := p.Requests[0]
	want := []string{"a", "b", OutputFormatPrompt + compactSchema(t)}
	if !slices.Equal(req.System, want) {
		t.Errorf("System = %q, want %q", req.System, want)
	}
	if req.CacheBoundary != 1 {
		t.Errorf("CacheBoundary = %d, want 1", req.CacheBoundary)
	}
}

func TestNoOutputSchemaNoSegment(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "hi"})
	system := []string{"a", "b"}
	a := mustNew(t, Config{Provider: p, System: system, CacheBoundary: 2})
	collect(t, a.Run(context.Background(), userInput("go")))
	req := p.Requests[0]
	if !slices.Equal(req.System, system) {
		t.Errorf("System = %q, want %q", req.System, system)
	}
	if req.CacheBoundary != 2 {
		t.Errorf("CacheBoundary = %d, want 2", req.CacheBoundary)
	}
}
