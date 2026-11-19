package llm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"
)

// sourceKey is the Block.ProviderData key under which a fallback provider
// records the name of the entry that produced a reply block.
const sourceKey = "orchestrix.source"

// DefaultFallbackCooldown is how long an entry that failed with a fallback
// error is skipped when FallbackConfig.Cooldown is zero.
const DefaultFallbackCooldown = 30 * time.Second

// FallbackEntry is one provider in a fallback chain.
type FallbackEntry struct {
	Provider Provider
	// Model is the model this provider answers with. When it matches a
	// registered profile, MaxTokens is lowered to that profile's MaxOutput;
	// a model matching no profile leaves MaxTokens unchanged. Optional: for
	// a backup built by New it defaults to the configured model. A custom
	// Provider without Model is never clamped.
	Model string
	// Name identifies the entry in events and in the "orchestrix.source"
	// stamp on its replies; it must stay the same across runs that share a
	// conversation. Explicit names must be unique. Default: Model, else
	// "fallback-<index>", with "-<index>" appended when that name is
	// already taken.
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
// error), or an error reported inside a stream (an *APIError with Status 0)
// whose JSON body has a "type" or "code" of overloaded_error,
// rate_limit_error, api_error, server_error or rate_limit_exceeded. A context
// overflow and a cancelled or expired context never qualify.
func DefaultShouldFallback(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || IsContextOverflow(err) {
		return false
	}
	return IsRetryable(err) || isTransientStreamError(err)
}

// NewFallback returns a Provider that tries cfg.Entries in order, moving to
// the next entry only when ShouldFallback accepts the error. An entry that
// failed that way is skipped for the cool-down period unless every entry is
// cooling down. Backups get the request with Model cleared, so they answer
// with the model they were built with. When the call's context is done, the
// error is returned as is: no switch, no OnFallback and no cool-down. When
// every entry fails, the last entry's error is returned wrapped (errors.Is
// and errors.As still reach it).
//
// Every block of a reply is stamped with ProviderData["orchestrix.source"]
// set to the answering entry's name. Before a request goes to an entry,
// assistant blocks stamped by another entry (unstamped blocks count as the
// primary's) are rewritten: thinking blocks are dropped, and other blocks
// lose their Signature and every other ProviderData key. The caller's
// messages are never modified. Stream switches only before the attempt has
// yielded content; see Stream. The returned Provider is safe for concurrent
// use.
func NewFallback(cfg FallbackConfig) (Provider, error) {
	if len(cfg.Entries) < 2 {
		return nil, fmt.Errorf("llm: fallback needs at least 2 entries, got %d", len(cfg.Entries))
	}
	entries := make([]FallbackEntry, len(cfg.Entries))
	taken := make(map[string]bool, len(entries))
	for i, e := range cfg.Entries {
		if e.Provider == nil {
			return nil, fmt.Errorf("llm: fallback entry %d has no provider", i)
		}
		if e.Name != "" {
			if taken[e.Name] {
				return nil, fmt.Errorf("llm: fallback entry name %q is used twice", e.Name)
			}
			taken[e.Name] = true
		}
		// A backup answers with the model it was built with; the primary
		// keeps the request's model, which may differ, so it is not filled.
		if r, ok := e.Provider.(modelReporter); ok && e.Model == "" && i > 0 {
			e.Model = r.configuredModel()
		}
		entries[i] = e
	}
	for i := range entries {
		if entries[i].Name != "" {
			continue
		}
		name := entries[i].Model
		if name == "" {
			name = fmt.Sprintf("fallback-%d", i)
		}
		if taken[name] {
			name = fmt.Sprintf("%s-%d", name, i)
		}
		if taken[name] {
			return nil, fmt.Errorf("llm: fallback entry %d: default name %q is already used", i, name)
		}
		taken[name] = true
		entries[i].Name = name
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

// modelReporter is implemented by the providers New builds, so NewFallback
// can fill a backup's FallbackEntry.Model.
type modelReporter interface {
	configuredModel() string
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
// lowered to the MaxOutput of the profile the entry's Model matches, if any.
// Assistant blocks from another entry are rewritten by provenance. req
// itself, its messages and their blocks are never modified.
func (f *fallback) prepare(i int, req Request) Request {
	if i > 0 {
		req.Model = ""
	}
	req.Messages = f.provenance(i, req.Messages)
	if m := f.entries[i].Model; m != "" && req.MaxTokens > 0 {
		if p, ok := lookupProfile(m); ok && p.MaxOutput > 0 && p.MaxOutput < req.MaxTokens {
			req.MaxTokens = p.MaxOutput
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
		resp, err := f.entries[i].Provider.Complete(f.entryContext(ctx, order, k), f.prepare(i, req))
		if err == nil {
			f.markUp(i)
			stamp(resp, f.entries[i].Name)
			return resp, nil
		}
		if !f.should(err) || ctx.Err() != nil {
			// A call whose context is done stops here: no switch and no
			// cool-down, since the entry may be healthy.
			return nil, err
		}
		last = err
		f.failed(ctx, order, k, err)
	}
	return nil, allFailed(last)
}

// entryContext is the context for the k-th entry of order. An entry that
// another follows gets a hand-off hint (withHandoff): providers built by New
// then return a fallback error at once instead of retrying it, so the switch
// happens without waiting through that entry's retries and Retry-After. The
// last entry keeps its normal retries, so a chain never gives up sooner than
// a single provider would.
func (f *fallback) entryContext(ctx context.Context, order []int, k int) context.Context {
	if k == len(order)-1 {
		return ctx
	}
	return withHandoff(ctx, f.should)
}

// stamp marks every block of resp as produced by the entry called name. Each
// ProviderData map is copied first, so a map the provider shares elsewhere is
// never written.
func stamp(resp *Response, name string) {
	if resp == nil {
		return
	}
	for i := range resp.Message.Content {
		b := &resp.Message.Content[i]
		pd := make(map[string]string, len(b.ProviderData)+1)
		maps.Copy(pd, b.ProviderData)
		pd[sourceKey] = name
		b.ProviderData = pd
	}
}

// provenance returns msgs as entry i may see them. In assistant messages, a
// block whose source (its sourceKey stamp, or the primary when unstamped) is
// not entry i is rewritten: a thinking block is dropped, any other block loses
// its Signature and every ProviderData key but sourceKey. Other roles carry
// no provider data and pass through. msgs is never modified: a message or
// block slice is copied only when something in it changes, and unchanged
// messages are shared with the result.
func (f *fallback) provenance(i int, msgs []Message) []Message {
	var out []Message // nil until the first message changes
	for k, m := range msgs {
		if m.Role == RoleAssistant {
			if blocks, changed := f.foreignBlocks(i, m.Content); changed {
				if out == nil {
					out = make([]Message, k, len(msgs))
					copy(out, msgs[:k])
				}
				m.Content = blocks
			}
		}
		if out != nil {
			out = append(out, m)
		}
	}
	if out == nil {
		return msgs
	}
	return out
}

// foreignBlocks applies the provenance rules to blocks for entry i. It
// returns blocks itself and false when nothing changes, else a new slice.
func (f *fallback) foreignBlocks(i int, blocks []Block) ([]Block, bool) {
	name, primary := f.entries[i].Name, f.entries[0].Name
	var out []Block // nil until the first block changes
	for k, b := range blocks {
		src := b.ProviderData[sourceKey]
		if src == "" {
			src = primary
		}
		keep, changed := true, false
		if src != name {
			switch {
			case b.Type == BlockThinking:
				keep, changed = false, true
			case b.Signature != "" || hasOtherKeys(b.ProviderData):
				b.Signature = ""
				b.ProviderData = onlySource(b.ProviderData)
				changed = true
			}
		}
		if changed && out == nil {
			out = make([]Block, k, len(blocks))
			copy(out, blocks[:k])
		}
		if out != nil && keep {
			out = append(out, b)
		}
	}
	if out == nil {
		return blocks, false
	}
	return out, true
}

// hasOtherKeys reports whether pd holds any key but sourceKey.
func hasOtherKeys(pd map[string]string) bool {
	for k := range pd {
		if k != sourceKey {
			return true
		}
	}
	return false
}

// onlySource returns a new map holding just pd's sourceKey, or nil when pd
// has none.
func onlySource(pd map[string]string) map[string]string {
	src, ok := pd[sourceKey]
	if !ok {
		return nil
	}
	return map[string]string{sourceKey: src}
}
