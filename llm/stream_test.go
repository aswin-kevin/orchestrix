package llm

import (
	"encoding/json"
	"testing"
)

func feed(t *testing.T, events ...StreamEvent) *Response {
	t.Helper()
	var a Accumulator
	for _, ev := range events {
		if err := a.Add(ev); err != nil {
			t.Fatalf("Add(%+v): %v", ev, err)
		}
	}
	resp, err := a.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	return resp
}

func TestAccumulatorAssemblesInterleavedBlocks(t *testing.T) {
	resp := feed(t,
		StreamEvent{Type: EventTextDelta, Index: 0, Text: "Hel"},
		StreamEvent{Type: EventTextDelta, Index: 0, Text: "lo"},
		StreamEvent{Type: EventToolStart, Index: 1, ToolID: "t1", ToolName: "read_file"},
		StreamEvent{Type: EventToolInputDelta, Index: 1, Text: `{"pa`},
		StreamEvent{Type: EventToolInputDelta, Index: 1, Text: `th":"/a"}`},
		StreamEvent{Type: EventThinkingDelta, Index: 2, Text: "hmm"},
		StreamEvent{Type: EventSignature, Index: 2, Value: "sig"},
		StreamEvent{Type: EventProviderData, Index: 1, Key: "k", Value: "v"},
		StreamEvent{Type: EventMessageDelta, StopReason: StopToolUse, Usage: Usage{Output: 9}},
		StreamEvent{Type: EventMessageStop},
	)
	c := resp.Message.Content
	if len(c) != 3 {
		t.Fatalf("got %d blocks, want 3: %+v", len(c), c)
	}
	if resp.Message.Role != RoleAssistant {
		t.Errorf("role = %q, want assistant", resp.Message.Role)
	}
	if c[0].Type != BlockText || c[0].Text != "Hello" {
		t.Errorf("block 0 = %+v", c[0])
	}
	if c[1].Type != BlockToolUse || c[1].ID != "t1" || c[1].Name != "read_file" ||
		string(c[1].Input) != `{"path":"/a"}` || c[1].ProviderData["k"] != "v" {
		t.Errorf("block 1 = %+v", c[1])
	}
	if c[2].Type != BlockThinking || c[2].Thinking != "hmm" || c[2].Signature != "sig" {
		t.Errorf("block 2 = %+v", c[2])
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop reason = %q", resp.StopReason)
	}
	if resp.Usage.Output != 9 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestAccumulatorOrdersBlocksByFirstSeenIndex(t *testing.T) {
	resp := feed(t,
		StreamEvent{Type: EventTextDelta, Index: 5, Text: "x"},
		StreamEvent{Type: EventToolStart, Index: 2, ToolID: "t", ToolName: "n"},
		StreamEvent{Type: EventTextDelta, Index: 5, Text: "y"},
	)
	c := resp.Message.Content
	if len(c) != 2 || c[0].Text != "xy" || c[1].Type != BlockToolUse {
		t.Errorf("content = %+v", c)
	}
}

func TestAccumulatorMergesUsageEchoes(t *testing.T) {
	resp := feed(t,
		StreamEvent{Type: EventMessageStart, Usage: Usage{Input: 10, CacheRead: 4}},
		StreamEvent{Type: EventMessageDelta, Usage: Usage{Input: 10, Output: 5}},
		StreamEvent{Type: EventMessageDelta, Usage: Usage{Output: 7}, StopReason: StopEndTurn},
	)
	want := Usage{Input: 10, Output: 12, CacheRead: 4}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestAccumulatorEmptyToolInputBecomesObject(t *testing.T) {
	resp := feed(t, StreamEvent{Type: EventToolStart, Index: 0, ToolID: "t1", ToolName: "n"})
	if got := string(resp.Message.Content[0].Input); got != `{}` {
		t.Errorf("Input = %q, want {}", got)
	}
}

func TestAccumulatorInvalidToolJSONIsKept(t *testing.T) {
	resp := feed(t,
		StreamEvent{Type: EventToolStart, Index: 0, ToolID: "t1", ToolName: "n"},
		StreamEvent{Type: EventToolInputDelta, Index: 0, Text: `{"a":`},
	)
	in := resp.Message.Content[0].Input
	if string(in) != `{"a":` {
		t.Errorf("Input = %q, want raw bytes kept", in)
	}
	if json.Valid(in) {
		t.Error("test premise broken: input should be invalid JSON")
	}
}

func TestAccumulatorRejectsDeltaForWrongBlockKind(t *testing.T) {
	var a Accumulator
	if err := a.Add(StreamEvent{Type: EventTextDelta, Index: 0, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Add(StreamEvent{Type: EventToolInputDelta, Index: 0, Text: "{}"}); err == nil {
		t.Error("expected error for tool input delta on a text block")
	}
}

func TestAccumulatorRejectsUnknownEvent(t *testing.T) {
	var a Accumulator
	if err := a.Add(StreamEvent{Type: "bogus"}); err == nil {
		t.Error("expected error for unknown event type")
	}
}

func TestAccumulatorResponseWithoutContentFails(t *testing.T) {
	var a Accumulator
	if _, err := a.Response(); err == nil {
		t.Error("expected error for empty stream")
	}
}
