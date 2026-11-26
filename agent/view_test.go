package agent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

func TestViewMatchesRequest(t *testing.T) {
	note := llm.UserText("first")
	note.Meta = map[string]string{"origin": "host"}
	dangling := llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{
		{Type: llm.BlockToolUse, ID: "old", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)},
	}}
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a := mustNew(t, Config{Provider: p})

	_, final := collect(t, a.Run(t.Context(), Input{Messages: []llm.Message{note, dangling, llm.UserText("again")}}))

	history := final.State.Messages()
	sent := history[:len(history)-1] // the reply came after the request
	view := View(sent)
	if !reflect.DeepEqual(view, p.Requests[0].Messages) {
		t.Fatalf("View = %+v\nrequest = %+v", view, p.Requests[0].Messages)
	}
	if view[0].Meta != nil {
		t.Fatalf("view kept Meta %v", view[0].Meta)
	}
	repaired := view[2].Content[0]
	if repaired.Type != llm.BlockToolResult || repaired.ToolUseID != "old" || !repaired.IsError {
		t.Fatalf("view[2] leads with %+v, want an interrupted result for the dangling call", repaired)
	}

	view[0].Content[0].Text = "changed"
	view[1].Content[0].Input[2] = 'X'
	if got := final.State.Messages(); got[0].Text() != "first" || string(got[1].Content[0].Input) != `{"text":"x"}` || got[0].Meta["origin"] != "host" {
		t.Fatalf("editing the view changed State: %+v", got[:2])
	}
}

func TestPendingHelper(t *testing.T) {
	log := newCallLog()
	_, events, final := interruptedRun(t, Config{Tools: []tool.Tool{readTool(log), writeTool(log)}},
		readCall("r1"), writeCall("w1"), writeCall("w2"))
	want := interruptEvent(t, events)

	got, ok := Pending(final.State)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Pending = %+v, %v; want %+v", got, ok, want)
	}

	data, err := json.Marshal(final.State)
	if err != nil {
		t.Fatal(err)
	}
	restored := state.New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatal(err)
	}
	got, ok = Pending(restored)
	if !ok || !reflect.DeepEqual(requestIDs(got), []string{"w1", "w2"}) || !reflect.DeepEqual(got, want) {
		t.Fatalf("Pending after JSON = %+v, %v; want %+v", got, ok, want)
	}

	if got, ok := Pending(state.New()); ok || got != nil {
		t.Fatalf("Pending(fresh) = %+v, true; want nil, false", got)
	}
	if got, ok := Pending(nil); ok || got != nil {
		t.Fatalf("Pending(nil) = %+v, true; want nil, false", got)
	}
}
