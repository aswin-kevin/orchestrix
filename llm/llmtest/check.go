package llmtest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
)

// cancelWait bounds how long CheckProvider waits for a cancelled stream to
// end.
const cancelWait = 5 * time.Second

// CheckProvider checks that p honours the llm.Provider contract for req. It
// verifies, in order, that Stream yields at least one event and no error;
// that every event is accepted by an llm.Accumulator; that Complete returns
// the same Message, StopReason and Usage as the collected stream; and that
// Stream with an already-cancelled context ends within five seconds, its
// first error matching context.Canceled and nothing yielded after it.
//
// It calls Stream and Complete with req, so it is meant for deterministic
// providers such as scripted backends and httptest servers, which answer the
// same request the same way each time. It reports through t.Errorf only.
func CheckProvider(t testing.TB, p llm.Provider, req llm.Request) {
	t.Helper()
	want, ok := checkStream(t, p, req)
	if !ok {
		return
	}
	got, err := p.Complete(t.Context(), req)
	switch {
	case err != nil:
		t.Errorf("llmtest: Complete failed: %v", err)
	case got == nil:
		t.Errorf("llmtest: Complete returned a nil Response and no error")
	default:
		if !reflect.DeepEqual(got.Message, want.Message) {
			t.Errorf("llmtest: Complete Message = %+v, collected stream = %+v", got.Message, want.Message)
		}
		if got.StopReason != want.StopReason {
			t.Errorf("llmtest: Complete StopReason = %q, collected stream = %q", got.StopReason, want.StopReason)
		}
		if !reflect.DeepEqual(got.Usage, want.Usage) {
			t.Errorf("llmtest: Complete Usage = %+v, collected stream = %+v", got.Usage, want.Usage)
		}
	}
	checkCancelled(t, p, req)
}

// checkStream runs the stream checks and returns the collected response.
func checkStream(t testing.TB, p llm.Provider, req llm.Request) (*llm.Response, bool) {
	t.Helper()
	var acc llm.Accumulator
	events := 0
	for ev, err := range p.Stream(t.Context(), req) {
		if err != nil {
			t.Errorf("llmtest: Stream returned an error: %v", err)
			return nil, false
		}
		events++
		if err := acc.Add(ev); err != nil {
			t.Errorf("llmtest: Stream event %d (%s) not accepted by Accumulator: %v", events, ev.Type, err)
			return nil, false
		}
	}
	if events == 0 {
		t.Errorf("llmtest: Stream yielded no events")
		return nil, false
	}
	resp, err := acc.Response()
	if err != nil {
		t.Errorf("llmtest: Stream events do not assemble into a Response: %v", err)
		return nil, false
	}
	return resp, true
}

// checkCancelled checks that a stream started with a cancelled context ends
// with context.Canceled. The first error counts; anything yielded after it
// is flagged, since an error ends the sequence.
func checkCancelled(t testing.TB, p llm.Provider, req llm.Request) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	type outcome struct {
		err  error
		more bool // something was yielded after err
	}
	result := make(chan outcome, 1)
	go func() {
		var o outcome
		for _, err := range p.Stream(ctx, req) {
			if o.err != nil {
				o.more = true
				continue
			}
			o.err = err
		}
		result <- o
	}()
	select {
	case o := <-result:
		if !errors.Is(o.err, context.Canceled) {
			t.Errorf("llmtest: Stream with a cancelled context ended with %v, want context.Canceled", o.err)
		}
		if o.more {
			t.Errorf("llmtest: Stream with a cancelled context yielded more after its error")
		}
	case <-time.After(cancelWait):
		t.Errorf("llmtest: Stream with a cancelled context did not end within %s", cancelWait)
	}
}
