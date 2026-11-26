package llm

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// optionsServer records each request body and answers 400, which is enough
// to inspect what an adapter sent.
func optionsServer(t *testing.T) (url string, bodies *[]map[string]json.RawMessage, hits *atomic.Int32) {
	t.Helper()
	var got []map[string]json.RawMessage
	var mu sync.Mutex
	hits = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &got, hits
}

type formatCase struct {
	name string
	make func(t *testing.T, url string) Provider
}

func optionFormats() []formatCase {
	return []formatCase{
		{"anthropic", func(t *testing.T, u string) Provider { return newTestAnthropic(t, u) }},
		{"openai", func(t *testing.T, u string) Provider { return newTestOpenAI(t, u) }},
		{"responses", func(t *testing.T, u string) Provider { return newTestResponses(t, u) }},
	}
}

var searchTool = []ToolSchema{{Name: "search", InputSchema: map[string]any{"type": "object"}}}

func optionRequest() Request {
	return Request{Messages: []Message{UserText("x")}, Tools: searchTool}
}

// Extra is checked by top-level key only: a nested map passes through
// unchanged even when its keys name fields the adapter set, and a nested
// value under a key the adapter set is still a conflict.
func TestExtraNestedKeys(t *testing.T) {
	for _, f := range optionFormats() {
		t.Run(f.name, func(t *testing.T) {
			url, bodies, _ := optionsServer(t)
			req := optionRequest()
			req.Extra = map[string]any{"model": map[string]any{"name": "x"}}
			if _, err := f.make(t, url).Complete(t.Context(), req); !errors.Is(err, ErrExtraConflict) {
				t.Fatalf("nested value under \"model\": err = %v, want ErrExtraConflict", err)
			}
			req.Extra = map[string]any{"metadata": map[string]any{"model": "x", "stream": true}}
			_, _ = f.make(t, url).Complete(t.Context(), req)
			if len(*bodies) != 1 {
				t.Fatalf("got %d requests, want 1", len(*bodies))
			}
			if got, want := string((*bodies)[0]["metadata"]), `{"model":"x","stream":true}`; got != want {
				t.Errorf("metadata = %s, want %s", got, want)
			}
			if got := string((*bodies)[0]["model"]); got == `"x"` {
				t.Errorf("model = %s, want the nested key not to reach the top level", got)
			}
		})
	}
}

func TestToolChoiceWire(t *testing.T) {
	want := map[string]map[string]string{
		"anthropic": {
			"auto": `{"type":"auto"}`, "any": `{"type":"any"}`, "none": `{"type":"none"}`,
			"tool": `{"name":"search","type":"tool"}`,
		},
		"openai": {
			"auto": `"auto"`, "any": `"required"`, "none": `"none"`,
			"tool": `{"function":{"name":"search"},"type":"function"}`,
		},
		"responses": {
			"auto": `"auto"`, "any": `"required"`, "none": `"none"`,
			"tool": `{"name":"search","type":"function"}`,
		},
	}
	modes := map[string]ToolChoice{
		"auto": {Mode: ToolChoiceAuto}, "any": {Mode: ToolChoiceAny},
		"none": {Mode: ToolChoiceNone}, "tool": {Mode: ToolChoiceTool, Name: "search"},
	}
	for _, f := range optionFormats() {
		for name, tc := range modes {
			t.Run(f.name+"/"+name, func(t *testing.T) {
				url, bodies, _ := optionsServer(t)
				req := optionRequest()
				req.ToolChoice = tc
				_, _ = f.make(t, url).Complete(t.Context(), req)
				if len(*bodies) != 1 {
					t.Fatalf("got %d requests, want 1", len(*bodies))
				}
				var v any
				if err := json.Unmarshal((*bodies)[0]["tool_choice"], &v); err != nil {
					t.Fatal(err)
				}
				got, _ := json.Marshal(v) // sorted keys
				if string(got) != want[f.name][name] {
					t.Errorf("tool_choice = %s, want %s", got, want[f.name][name])
				}
			})
		}
		t.Run(f.name+"/zero", func(t *testing.T) {
			url, bodies, _ := optionsServer(t)
			_, _ = f.make(t, url).Complete(t.Context(), optionRequest())
			if _, ok := (*bodies)[0]["tool_choice"]; ok {
				t.Error("tool_choice present for zero ToolChoice")
			}
		})
	}
}

func TestToolChoiceInvalid(t *testing.T) {
	tests := []struct {
		name  string
		tc    ToolChoice
		tools []ToolSchema
		want  string
	}{
		{"unknown mode", ToolChoice{Mode: "bogus"}, searchTool, `unknown tool choice mode "bogus"`},
		{"name without tool mode", ToolChoice{Mode: ToolChoiceAuto, Name: "search"}, searchTool, `tool choice name "search" needs mode "tool"`},
		{"any without tools", ToolChoice{Mode: ToolChoiceAny}, nil, `tool choice "any" needs tools`},
		{"tool without tools", ToolChoice{Mode: ToolChoiceTool, Name: "search"}, nil, `tool choice "tool" needs tools`},
		{"unknown tool", ToolChoice{Mode: ToolChoiceTool, Name: "missing"}, searchTool, `tool choice names unknown tool "missing"`},
	}
	for _, f := range optionFormats() {
		for _, tt := range tests {
			t.Run(f.name+"/"+tt.name, func(t *testing.T) {
				url, _, hits := optionsServer(t)
				req := optionRequest()
				req.Tools, req.ToolChoice = tt.tools, tt.tc
				_, err := f.make(t, url).Complete(t.Context(), req)
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("err = %v, want containing %q", err, tt.want)
				}
				if n := strings.Count(err.Error(), "llm: "); n != 1 {
					t.Errorf("err = %q has %d \"llm: \" prefixes, want 1", err, n)
				}
				if hits.Load() != 0 {
					t.Error("server was hit")
				}
			})
		}
	}
}

func TestExtraMerged(t *testing.T) {
	for _, f := range optionFormats() {
		t.Run(f.name, func(t *testing.T) {
			url, bodies, _ := optionsServer(t)
			req := optionRequest()
			req.Extra = map[string]any{"top_p": 0.5}
			_, _ = f.make(t, url).Complete(t.Context(), req)
			if got := string((*bodies)[0]["top_p"]); got != "0.5" {
				t.Errorf("top_p = %q, want 0.5", got)
			}
			for _, key := range []string{"model", "stream"} {
				if _, ok := (*bodies)[0][key]; !ok {
					t.Errorf("adapter field %q lost in merge", key)
				}
			}
			carrier := map[string]string{"anthropic": "messages", "openai": "messages", "responses": "input"}[f.name]
			if _, ok := (*bodies)[0][carrier]; !ok {
				t.Errorf("adapter field %q lost in merge", carrier)
			}
		})
	}
}

func TestExtraNested(t *testing.T) {
	for _, f := range optionFormats() {
		t.Run(f.name, func(t *testing.T) {
			url, bodies, _ := optionsServer(t)
			req := optionRequest()
			req.Extra = map[string]any{"metadata": map[string]any{"ids": []int{1, 2}, "k": "v"}}
			_, _ = f.make(t, url).Complete(t.Context(), req)
			if got := string((*bodies)[0]["metadata"]); got != `{"ids":[1,2],"k":"v"}` {
				t.Errorf("metadata = %s", got)
			}
		})
	}
}

func TestExtraConflict(t *testing.T) {
	for _, f := range optionFormats() {
		t.Run(f.name, func(t *testing.T) {
			url, bodies, hits := optionsServer(t)
			req := optionRequest()
			for _, key := range []string{"model", "stream"} {
				req.Extra = map[string]any{key: "x"}
				_, err := f.make(t, url).Complete(t.Context(), req)
				if !errors.Is(err, ErrExtraConflict) {
					t.Fatalf("%s: err = %v, want ErrExtraConflict", key, err)
				}
			}
			if hits.Load() != 0 {
				t.Error("server was hit")
			}
			req.Extra = map[string]any{"temperature": 1}
			_, _ = f.make(t, url).Complete(t.Context(), req)
			if len(*bodies) != 1 || string((*bodies)[0]["temperature"]) != "1" {
				t.Errorf("temperature extra not sent: %v", *bodies)
			}
		})
	}
}
