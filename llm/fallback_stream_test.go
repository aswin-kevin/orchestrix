package llm_test

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// eventStub is a Provider whose Stream yields its events and then, when set,
// its error. Complete collects the stream.
type eventStub struct {
	events []llm.StreamEvent
	err    error
	mu     sync.Mutex
	calls  int
}

func (s *eventStub) Stream(context.Context, llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		for _, ev := range s.events {
			if !yield(ev, nil) {
				return
			}
		}
		if s.err != nil {
			yield(llm.StreamEvent{}, s.err)
		}
	}
}

func (s *eventStub) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, s, req)
}

func (s *eventStub) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// drain consumes p.Stream and returns every event and the first error.
func drain(p llm.Provider, req llm.Request) ([]llm.StreamEvent, error) {
	var evs []llm.StreamEvent
	for ev, err := range p.Stream(context.Background(), req) {
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

func countType(evs []llm.StreamEvent, t llm.EventType) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == t {
			n++
		}
	}
	return n
}

func TestFallbackStreamSwitchesBeforeContent(t *testing.T) {
	backup := llmtest.New(llmtest.Turn{Text: "ok"})
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: llmtest.New(overloaded()), Name: "a"},
		{Provider: backup, Name: "b"},
	}})
	resp, err := llm.Collect(context.Background(), p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Message.Text(); got != "ok" {
		t.Errorf("text = %q, want ok", got)
	}
	if backup.Calls() != 1 {
		t.Errorf("backup calls = %d, want 1", backup.Calls())
	}
}

func TestFallbackStreamDropsHeldEvents(t *testing.T) {
	primary := &eventStub{
		events: []llm.StreamEvent{{Type: llm.EvMessageStart, Usage: llm.Usage{Input: 99}}},
		err:    &llm.APIError{Status: 529},
	}
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: primary, Name: "a"},
		{Provider: llmtest.New(llmtest.Turn{Text: "ok", Usage: llm.Usage{Input: 7}}), Name: "b"},
	}})
	evs, err := drain(p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(evs, llm.EvMessageStart); n != 1 {
		t.Fatalf("message starts = %d, want 1", n)
	}
	for _, ev := range evs {
		if ev.Type == llm.EvMessageStart && ev.Usage.Input != 7 {
			t.Errorf("message start usage = %d, want the backup's 7", ev.Usage.Input)
		}
	}
}

func TestFallbackStreamNoSwitchAfterContent(t *testing.T) {
	primary := &eventStub{
		events: []llm.StreamEvent{
			{Type: llm.EvMessageStart},
			{Type: llm.EvTextDelta, Text: "par"},
		},
		err: &llm.APIError{Status: 529},
	}
	backup := llmtest.New(llmtest.Turn{Text: "tial"})
	called := false
	p := newFallback(t, llm.FallbackConfig{
		Entries:    []llm.FallbackEntry{{Provider: primary, Name: "a"}, {Provider: backup, Name: "b"}},
		OnFallback: func(context.Context, llm.FallbackEvent) { called = true },
	})
	evs, err := drain(p, llm.Request{})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 529 {
		t.Fatalf("err = %v, want the primary's 529", err)
	}
	if backup.Calls() != 0 || called {
		t.Errorf("switched to the backup after content")
	}
	text := ""
	for _, ev := range evs {
		if ev.Type == llm.EvTextDelta {
			text += ev.Text
		}
	}
	if text != "par" {
		t.Errorf("text = %q, want par once", text)
	}
}

func TestFallbackStreamCoolsDownAfterMidStreamFailure(t *testing.T) {
	primary := &eventStub{
		events: []llm.StreamEvent{{Type: llm.EvTextDelta, Text: "par"}},
		err:    &llm.APIError{Status: 529},
	}
	backup := llmtest.New(llmtest.Turn{Text: "ok"})
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: primary}, {Provider: backup}}})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	llm.SetFallbackClock(p, func() time.Time { return now })

	if _, err := drain(p, llm.Request{}); err == nil {
		t.Fatal("first stream succeeded")
	}
	resp, err := llm.Collect(context.Background(), p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != "ok" || primary.Calls() != 1 {
		t.Errorf("text %q, primary calls %d; want ok and 1 (primary cooling down)", resp.Message.Text(), primary.Calls())
	}
}

func TestFallbackStreamNonFallbackErrorBeforeContent(t *testing.T) {
	primary := &eventStub{
		events: []llm.StreamEvent{{Type: llm.EvMessageStart}},
		err:    &llm.APIError{Status: 400},
	}
	backup := llmtest.New(llmtest.Turn{Text: "ok"})
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: primary}, {Provider: backup}}})
	_, err := drain(p, llm.Request{})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Errorf("err = %v, want the 400", err)
	}
	if backup.Calls() != 0 {
		t.Errorf("switched on a 400")
	}
}

func TestFallbackStreamWithoutContentFlushes(t *testing.T) {
	primary := &eventStub{events: []llm.StreamEvent{
		{Type: llm.EvMessageStart, Usage: llm.Usage{Input: 3}},
		{Type: llm.EvMessageDelta, StopReason: llm.StopEndTurn},
		{Type: llm.EvMessageStop},
	}}
	backup := llmtest.New(llmtest.Turn{Text: "ok"})
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: primary}, {Provider: backup}}})
	evs, err := drain(p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 || evs[0].Type != llm.EvMessageStart || evs[2].Type != llm.EvMessageStop {
		t.Errorf("events = %+v, want the primary's three events", evs)
	}
	if backup.Calls() != 0 {
		t.Errorf("backup called")
	}
}

func TestFallbackStreamStopsWhenConsumerStops(t *testing.T) {
	primary := &eventStub{events: []llm.StreamEvent{
		{Type: llm.EvMessageStart},
		{Type: llm.EvTextDelta, Text: "a"},
		{Type: llm.EvTextDelta, Text: "b"},
	}}
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{{Provider: primary}, {Provider: llmtest.New()}}})
	n := 0
	for range p.Stream(context.Background(), llm.Request{}) {
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Errorf("events seen = %d, want 2", n)
	}
}

func TestFallbackStreamStampsBlocks(t *testing.T) {
	// A block may start with a non-content event, as Anthropic's redacted
	// thinking does: the stamp follows it and is held back with it.
	primary := &eventStub{events: []llm.StreamEvent{
		{Type: llm.EvMessageStart},
		{Type: llm.EvSignature, Index: 0, Value: "opaque"},
		{Type: llm.EvTextDelta, Index: 1, Text: "hi"},
		{Type: llm.EvTextDelta, Index: 1, Text: "!"},
		{Type: llm.EvMessageStop},
	}}
	p := newFallback(t, llm.FallbackConfig{Entries: []llm.FallbackEntry{
		{Provider: primary, Name: "a"},
		{Provider: llmtest.New(), Name: "b"},
	}})
	evs, err := drain(p, llm.Request{})
	if err != nil {
		t.Fatal(err)
	}
	stamps := map[int]int{}
	for k, ev := range evs {
		if ev.Type != llm.EvProviderData {
			continue
		}
		if ev.Key != "orchestrix.source" || ev.Value != "a" {
			t.Errorf("stamp = %+v", ev)
		}
		if k == 0 || evs[k-1].Index != ev.Index || evs[k-1].Type == llm.EvProviderData {
			t.Errorf("stamp for block %d does not follow the block's first event", ev.Index)
		}
		stamps[ev.Index]++
	}
	if stamps[0] != 1 || stamps[1] != 1 || len(stamps) != 2 {
		t.Errorf("stamps per block = %v, want one each for blocks 0 and 1", stamps)
	}
}
