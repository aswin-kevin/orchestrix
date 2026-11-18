package llm

import (
	"strings"
	"testing"
)

func TestReadSSEParsesEventsAndMultilineData(t *testing.T) {
	input := ": keep-alive\n" +
		"event: a\ndata: x\ndata: y\n\n" +
		"data: {\"n\":1}\r\n\r\n" +
		"event: last\ndata: tail"

	var got []sseEvent
	for ev, err := range readSSE(strings.NewReader(input)) {
		if err != nil {
			t.Fatalf("readSSE error: %v", err)
		}
		got = append(got, ev)
	}

	want := []sseEvent{
		{Event: "a", Data: "x\ny"},
		{Data: `{"n":1}`},
		{Event: "last", Data: "tail"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
