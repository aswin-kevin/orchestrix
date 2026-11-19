package llm

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"time"
)

// DefaultFallbackCooldown is how long an entry that failed with a fallback
// error is skipped when FallbackConfig.Cooldown is zero.
const DefaultFallbackCooldown = 30 * time.Second

// FallbackEntry is one provider in a fallback chain.
type FallbackEntry struct {
	Provider Provider
	// Model is the model this provider answers with: used to clamp
	// MaxTokens to its profile and to report switches. Optional.
	Model string
	// Name identifies the entry in events; it must be unique within the
	// chain. Default: Model, else "fallback-<index>".
	Name string
}

// FallbackEvent describes one switch from an entry to the next.
type FallbackEvent struct {
	// From and To are entry names.
	From, To string
	// Err is the error that caused the switch.
	Err error
}

// FallbackConfig configures NewFallback.
type FallbackConfig struct {
	// Entries are tried in order; the first is the primary. At least two
	// are required.
	Entries []FallbackEntry
	// Cooldown is how long an entry that failed with a fallback error is
	// skipped. 0 means DefaultFallbackCooldown; negative disables it.
	Cooldown time.Duration
	// ShouldFallback decides whether err moves the call to the next entry.
	// nil means DefaultShouldFallback.
	ShouldFallback func(error) bool
	// OnFallback, when set, is called synchronously on each switch with the
	// call's context, before the next entry is tried.
	OnFallback func(ctx context.Context, ev FallbackEvent)
}

// DefaultShouldFallback reports whether err means the provider cannot serve
// right now: a retryable failure (408, 409, 429, 5xx, 529 or a network
// error) that is not a context overflow. A cancelled context never qualifies.
func DefaultShouldFallback(err error) bool {
	return IsRetryable(err) && !IsContextOverflow(err)
}

// NewFallback returns a Provider that tries cfg.Entries in order, moving to
// the next entry only when ShouldFallback accepts the error. An entry that
// failed that way is skipped for the cool-down period unless every entry is
// cooling down. Backups get the request with Model cleared, so they answer
// with the model they were built with. The returned Provider is safe for
// concurrent use.
func NewFallback(cfg FallbackConfig) (Provider, error) {
	if len(cfg.Entries) < 2 {
		return nil, fmt.Errorf("llm: fallback needs at least 2 entries, got %d", len(cfg.Entries))
	}
	entries := make([]FallbackEntry, len(cfg.Entries))
	seen := make(map[string]bool, len(entries))
	for i, e := range cfg.Entries {
		if e.Provider == nil {
			return nil, fmt.Errorf("llm: fallback entry %d has no provider", i)
		}
		if e.Name == "" {
			e.Name = e.Model
		}
		if e.Name == "" {
			e.Name = fmt.Sprintf("fallback-%d", i)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("llm: fallback entry name %q is used twice", e.Name)
		}
		seen[e.Name] = true
		entries[i] = e
	}
	f := &fallback{
		entries:  entries,
		cooldown: cfg.Cooldown,
		should:   cfg.ShouldFallback,
		notify:   cfg.OnFallback,
		now:      time.Now,
		downTill: make([]time.Time, len(entries)),
	}
	if f.cooldown == 0 {
		f.cooldown = DefaultFallbackCooldown
	}
	if f.should == nil {
		f.should = DefaultShouldFallback
	}
	return f, nil
}

// fallback is the Provider returned by NewFallback.
type fallback struct {
	entries  []FallbackEntry
	cooldown time.Duration
	should   func(error) bool
	notify   func(context.Context, FallbackEvent)
	// now is the cool-down clock; tests replace it.
	now func() time.Time

	mu       sync.Mutex
	downTill []time.Time // per entry; zero means not cooling down
}

// order returns the indexes of the entries to try at now: those not cooling
// down, in order, or every entry when all are cooling down.
func (f *fallback) order(now time.Time) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := make([]int, 0, len(f.entries))
	for i, until := range f.downTill {
		if !now.Before(until) {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		for i := range f.entries {
			idx = append(idx, i)
		}
	}
	return idx
}

// markDown starts entry i's cool-down.
func (f *fallback) markDown(i int) {
	if f.cooldown < 0 {
		return
	}
	until := f.now().Add(f.cooldown)
	f.mu.Lock()
	f.downTill[i] = until
	f.mu.Unlock()
}

// markUp clears entry i's cool-down.
func (f *fallback) markUp(i int) {
	f.mu.Lock()
	f.downTill[i] = time.Time{}
	f.mu.Unlock()
}

// prepare returns the request sent to entry i. The primary keeps req.Model;
// other entries get it cleared so they use their own model. MaxTokens is
// lowered to the entry model's MaxOutput. req itself is never modified.
func (f *fallback) prepare(i int, req Request) Request {
	if i > 0 {
		req.Model = ""
	}
	if m := f.entries[i].Model; m != "" && req.MaxTokens > 0 {
		if limit := LookupProfile(m).MaxOutput; limit > 0 && limit < req.MaxTokens {
			req.MaxTokens = limit
		}
	}
	return req
}

// failed handles a fallback error from entry order[k]: it starts the entry's
// cool-down and reports the switch when another entry follows.
func (f *fallback) failed(ctx context.Context, order []int, k int, err error) {
	f.markDown(order[k])
	if f.notify != nil && k+1 < len(order) {
		f.notify(ctx, FallbackEvent{
			From: f.entries[order[k]].Name,
			To:   f.entries[order[k+1]].Name,
			Err:  err,
		})
	}
}

// allFailed wraps the last entry's error once every entry has failed.
func allFailed(err error) error {
	return fmt.Errorf("llm: every fallback entry failed: %w", err)
}

// Complete implements Provider.
func (f *fallback) Complete(ctx context.Context, req Request) (*Response, error) {
	order := f.order(f.now())
	var last error
	for k, i := range order {
		resp, err := f.entries[i].Provider.Complete(ctx, f.prepare(i, req))
		if err == nil {
			f.markUp(i)
			return resp, nil
		}
		if !f.should(err) {
			return nil, err
		}
		last = err
		f.failed(ctx, order, k, err)
	}
	return nil, allFailed(last)
}

// Stream implements Provider. It moves to the next entry only when an
// attempt's first item is a fallback error; any later error ends the stream.
func (f *fallback) Stream(ctx context.Context, req Request) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		order := f.order(f.now())
		var last error
		for k, i := range order {
			switched, err := f.streamEntry(ctx, i, req, yield)
			if !switched {
				return
			}
			last = err
			f.failed(ctx, order, k, err)
		}
		yield(StreamEvent{}, allFailed(last))
	}
}

// streamEntry relays entry i's stream to yield. It returns switched true,
// with the error, when the first item was a fallback error; nothing has been
// yielded then.
func (f *fallback) streamEntry(ctx context.Context, i int, req Request, yield func(StreamEvent, error) bool) (switched bool, err error) {
	first := true
	for ev, err := range f.entries[i].Provider.Stream(ctx, f.prepare(i, req)) {
		if err != nil && first && f.should(err) {
			return true, err
		}
		first = false
		if !yield(ev, err) || err != nil {
			return false, nil
		}
	}
	f.markUp(i)
	return false, nil
}
