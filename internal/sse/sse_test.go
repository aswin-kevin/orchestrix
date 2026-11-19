package sse

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

func TestEvents(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		max     int
		want    []Event
		wantErr error
	}{
		{
			name:  "multi-line data",
			input: "event: a\ndata: x\ndata: y\n\n",
			want:  []Event{{Event: "a", Data: "x\ny"}},
		},
		{
			name:  "comments and unknown fields skipped",
			input: ": keep-alive\nretry: 10\ndata: one\n: inside\n\n:ping\n\n",
			want:  []Event{{Data: "one"}},
		},
		{
			name:  "CRLF line endings",
			input: "event: b\r\ndata: {\"n\":1}\r\n\r\ndata: two\r\n\r\n",
			want:  []Event{{Event: "b", Data: `{"n":1}`}, {Data: "two"}},
		},
		{
			name:  "id persists across events",
			input: "id: 7\ndata: a\n\ndata: b\n\nid: 8\ndata: c\n\n",
			want:  []Event{{Data: "a", ID: "7"}, {Data: "b", ID: "7"}, {Data: "c", ID: "8"}},
		},
		{
			name:  "event type resets after dispatch",
			input: "event: x\ndata: 1\n\ndata: 2\n\n",
			want:  []Event{{Event: "x", Data: "1"}, {Data: "2"}},
		},
		{
			name:  "no space after colon and empty data",
			input: "data:tight\n\ndata\n\n",
			want:  []Event{{Data: "tight"}, {Data: ""}},
		},
		{
			name:  "events without data are not dispatched",
			input: "event: lonely\n\nid: 3\n\n",
		},
		{
			name:  "pending event emitted at EOF",
			input: "event: last\ndata: tail",
			want:  []Event{{Event: "last", Data: "tail"}},
		},
		{
			name:  "event at the limit",
			input: "data: 12345\n\n",
			max:   len("data: 12345"),
			want:  []Event{{Data: "12345"}},
		},
		{
			name:    "oversized event across lines",
			input:   "data: 12345\ndata: 67890\n\n",
			max:     16,
			wantErr: ErrTooLarge,
		},
		{
			name:    "oversized single line",
			input:   "data: " + strings.Repeat("x", 10000) + "\n\n",
			max:     100,
			wantErr: ErrTooLarge,
		},
		{
			name:    "oversized line without terminator",
			input:   ": " + strings.Repeat("x", 10000),
			max:     100,
			wantErr: ErrTooLarge,
		},
		{
			name:  "limit is per event",
			input: "data: 12345\n\ndata: 67890\n\n",
			max:   len("data: 12345"),
			want:  []Event{{Data: "12345"}, {Data: "67890"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			max := tt.max
			if max == 0 {
				max = 1 << 20
			}
			var got []Event
			var err error
			// One byte per read exercises lines split across buffer fills.
			for ev, e := range Events(iotest.OneByteReader(strings.NewReader(tt.input)), max) {
				if e != nil {
					err = e
					break
				}
				got = append(got, ev)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("events = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEventsLongLineWithinLimit(t *testing.T) {
	long := strings.Repeat("y", 100<<10) // spans many reader buffers
	var got []Event
	for ev, err := range Events(strings.NewReader("data: "+long+"\n\n"), 1<<20) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Data != long {
		t.Fatalf("got %d events, want one with %d bytes of data", len(got), len(long))
	}
}

func TestEventsReadError(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("data: a\n\n"), iotest.ErrReader(boom))
	var got []Event
	var err error
	for ev, e := range Events(r, 1<<20) {
		if e != nil {
			err = e
			break
		}
		got = append(got, ev)
	}
	if len(got) != 1 || !errors.Is(err, boom) {
		t.Fatalf("events = %+v, err = %v; want one event, then boom", got, err)
	}
}

func TestEventsStopsWhenConsumerStops(t *testing.T) {
	n := 0
	for range Events(strings.NewReader("data: 1\n\ndata: 2\n\ndata: 3\n\n"), 1<<20) {
		n++
		break
	}
	if n != 1 {
		t.Fatalf("consumed %d events, want 1", n)
	}
}

func TestEventsRejectsNonPositiveMax(t *testing.T) {
	for _, max := range []int{0, -1} {
		var err error
		for _, e := range Events(strings.NewReader("data: x\n\n"), max) {
			err = e
			break
		}
		if err == nil || !strings.Contains(err.Error(), "positive") {
			t.Errorf("Events(max %d) error = %v, want one saying max must be positive", max, err)
		}
	}
}
