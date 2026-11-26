package llm

import (
	"context"
	"iter"
)

// Stream implements Provider. An attempt's non-content events are held back
// until its first content event (a text, thinking or tool-input delta, or a
// tool start); then they are flushed and the call is committed to that entry.
// A fallback error before the commit drops the held events and moves to the
// next entry; any other error is returned. After the commit every error ends
// the stream as is, so text already yielded is never repeated or mixed with
// another model's; OnFallback is not called then, but a fallback error still
// starts the entry's cool-down so the next call begins elsewhere. An attempt
// that ends without content is committed when it ends. The first event of each
// new block is followed by an EventProviderData event stamping the block with
// the entry's name.
func (f *Fallback) Stream(ctx context.Context, req Request) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		order := f.order(f.now())
		var last error
		for k, i := range order {
			switched, err := f.streamEntry(f.entryContext(ctx, order, k), i, req, yield)
			if !switched {
				return
			}
			if ctx.Err() != nil {
				// The call's context is done: no switch and no cool-down.
				yield(StreamEvent{}, err)
				return
			}
			last = err
			f.failed(ctx, order, k, err)
		}
		yield(StreamEvent{}, allFailed(last))
	}
}

// streamEntry relays entry i's stream to yield under the rules of Stream. It
// returns switched true, with the error, when the attempt failed with a
// fallback error before committing; nothing has been yielded then.
func (f *Fallback) streamEntry(ctx context.Context, i int, req Request, yield func(StreamEvent, error) bool) (switched bool, err error) {
	s := entryStream{name: f.entries[i].Name, yield: yield}
	for ev, err := range f.entries[i].Provider.Stream(ctx, f.prepare(i, req)) {
		if err != nil {
			if !s.committed && f.should(err) {
				return true, err
			}
			if s.committed && f.should(err) {
				// Too late to switch, but the entry is failing: let the
				// next call start elsewhere.
				f.markDown(i)
			}
			yield(StreamEvent{}, err)
			return false, nil
		}
		if !s.add(ev) {
			return false, nil
		}
	}
	if !s.commit() {
		return false, nil
	}
	f.markUp(i)
	return false, nil
}

// entryStream holds back and stamps the events of one Stream attempt.
type entryStream struct {
	name      string
	yield     func(StreamEvent, error) bool
	held      []StreamEvent
	committed bool
	stamped   map[int]bool // block indexes already stamped
}

// add takes one event from the provider. It reports false when the consumer
// stopped.
func (s *entryStream) add(ev StreamEvent) bool {
	if !s.committed && isContentEvent(ev.Type) && !s.commit() {
		return false
	}
	if !s.emit(ev) {
		return false
	}
	if !startsBlock(ev.Type) || s.stamped[ev.Index] {
		return true
	}
	if s.stamped == nil {
		s.stamped = make(map[int]bool)
	}
	s.stamped[ev.Index] = true
	return s.emit(StreamEvent{Type: EventProviderData, Index: ev.Index, Key: sourceKey, Value: s.name})
}

// emit yields ev once committed and holds it back before.
func (s *entryStream) emit(ev StreamEvent) bool {
	if !s.committed {
		s.held = append(s.held, ev)
		return true
	}
	return s.yield(ev, nil)
}

// commit flushes the held events; later events are yielded directly. It
// reports false when the consumer stopped.
func (s *entryStream) commit() bool {
	if s.committed {
		return true
	}
	s.committed = true
	held := s.held
	s.held = nil
	for _, ev := range held {
		if !s.yield(ev, nil) {
			return false
		}
	}
	return true
}

// isContentEvent reports whether an event of type t shows output to the
// consumer, after which a stream can no longer switch entries.
func isContentEvent(t EventType) bool {
	switch t {
	case EventTextDelta, EventThinkingDelta, EventToolStart, EventToolInputDelta:
		return true
	}
	return false
}

// startsBlock reports whether an event of type t belongs to a content block,
// creating it in an Accumulator when its Index is new. EventProviderData does
// not: it needs its block to exist already.
func startsBlock(t EventType) bool {
	return isContentEvent(t) || t == EventSignature
}
