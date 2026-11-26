// Package elision holds the elisions of a run's State: ranges of messages the
// request view replaces with one user message. Package agent applies them
// when it builds a model request and shifts them when a resume inserts
// messages; agent/elide is the public API over them.
package elision

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// Elision replaces State messages [From, To) in the request view with one
// user message holding Text. agent/elide.Elision has the same fields and
// json tags, so the two convert into each other.
type Elision struct {
	// ID names the elision, unique within a State.
	ID string `json:"id"`
	// From is the index of the first State message replaced.
	From int `json:"from"`
	// To is the index just past the last State message replaced.
	To int `json:"to"`
	// Text is the content of the user message standing for the range.
	Text string `json:"text"`
}

// Key holds a run's elisions, sorted by From and non-overlapping when
// recorded by Add. Private (a subagent's history is its own); State.Fork
// copies it. Unset means no elisions.
var Key = state.NewKey[[]Elision]("agent.elisions", state.Private[[]Elision]())

// ErrInvalid is wrapped by the error Add returns for an elision it refuses.
// agent/elide exports it as elide.ErrInvalid.
var ErrInvalid = errors.New("elide: invalid elision")

// mu serializes the read-check-write of Add, Remove and Shift, so concurrent
// callers cannot record overlapping ranges.
var mu sync.Mutex

// Add records e after checking it against s: Check, a unique ID and no
// overlap with a recorded elision. Otherwise an error wrapping ErrInvalid,
// and s is unchanged.
func Add(s *state.State, e Elision) error {
	mu.Lock()
	defer mu.Unlock()
	if err := Check(s.Messages(), e); err != nil {
		return fmt.Errorf("%w %q: %v", ErrInvalid, e.ID, err)
	}
	current, _ := Key.Get(s)
	for _, other := range current {
		switch {
		case other.ID == e.ID:
			return fmt.Errorf("%w %q: duplicate id", ErrInvalid, e.ID)
		case overlaps(other, e):
			return fmt.Errorf("%w %q: [%d, %d) overlaps %q", ErrInvalid, e.ID, e.From, e.To, other.ID)
		}
	}
	updated := append(slices.Clone(current), e)
	slices.SortStableFunc(updated, byFrom)
	Key.Set(s, updated)
	return nil
}

// Remove deletes the elision with id; false when there is none. The key is
// cleared when its last elision goes.
func Remove(s *state.State, id string) bool {
	mu.Lock()
	defer mu.Unlock()
	current, _ := Key.Get(s)
	i := slices.IndexFunc(current, func(e Elision) bool { return e.ID == id })
	if i < 0 {
		return false
	}
	updated := slices.Delete(slices.Clone(current), i, i+1)
	if len(updated) == 0 {
		Key.Delete(s)
	} else {
		Key.Set(s, updated)
	}
	return true
}

// All returns a copy of the elisions stored in s, in stored order; nil when
// there are none.
func All(s *state.State) []Elision {
	current, _ := Key.Get(s)
	return slices.Clone(current)
}

// Clear deletes every elision of s.
func Clear(s *state.State) {
	mu.Lock()
	defer mu.Unlock()
	Key.Delete(s)
}

// View returns s.Messages()[from:] with every elision Applied(s, from)
// returns replaced by llm.UserText(e.Text). from is clamped to [0, len]. The
// result is not repaired; package agent's View does that.
func View(s *state.State, from int) []llm.Message {
	msgs := s.Messages()
	from = min(max(from, 0), len(msgs))
	applied := applied(s, msgs, from)
	out := make([]llm.Message, 0, len(msgs)-from)
	for i := from; i < len(msgs); i++ {
		if len(applied) > 0 && applied[0].From == i {
			out = append(out, llm.UserText(applied[0].Text))
			i = applied[0].To - 1
			applied = applied[1:]
			continue
		}
		out = append(out, msgs[i])
	}
	return out
}

// Applied returns the elisions of s that View(s, from) applies, sorted by
// From: those lying wholly within [from, len(messages)) that pass Check and
// do not overlap one kept before them. from is clamped to [0, len].
func Applied(s *state.State, from int) []Elision {
	msgs := s.Messages()
	return applied(s, msgs, min(max(from, 0), len(msgs)))
}

// applied is Applied over msgs with from already clamped.
func applied(s *state.State, msgs []llm.Message, from int) []Elision {
	all, _ := Key.Get(s)
	if len(all) == 0 {
		return nil
	}
	sorted := slices.Clone(all)
	slices.SortStableFunc(sorted, byFrom)
	var kept []Elision
	for _, e := range sorted {
		if e.From < from || Check(msgs, e) != nil {
			continue
		}
		if len(kept) > 0 && overlaps(kept[len(kept)-1], e) {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// Shift moves the elisions of s past n messages inserted at index at: one
// starting at or after at moves whole, and one spanning at grows to include
// the inserted messages.
func Shift(s *state.State, at, n int) {
	mu.Lock()
	defer mu.Unlock()
	current, _ := Key.Get(s)
	if !slices.ContainsFunc(current, func(e Elision) bool { return e.To > at }) {
		return
	}
	updated := slices.Clone(current)
	for i, e := range updated {
		if e.From >= at {
			updated[i].From += n
		}
		if e.To > at {
			updated[i].To += n
		}
	}
	Key.Set(s, updated)
}

// Check reports why e does not fit msgs, or nil when it does: non-empty ID
// and Text, 0 <= From < To <= len(msgs), and a self-contained range. It does
// not check e against other elisions.
func Check(msgs []llm.Message, e Elision) error {
	switch {
	case e.ID == "":
		return errors.New("empty id")
	case e.Text == "":
		return errors.New("empty text")
	case e.From < 0 || e.From >= e.To || e.To > len(msgs):
		return fmt.Errorf("range [%d, %d) is not within the %d messages", e.From, e.To, len(msgs))
	}
	return selfContained(msgs[e.From:e.To])
}

// selfContained reports a tool_use in msgs not answered within them, or a
// tool_result in msgs answering a call outside them.
func selfContained(msgs []llm.Message) error {
	calls := make(map[string]bool)
	answered := make(map[string]bool)
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockToolUse:
				calls[b.ID] = true
			case llm.BlockToolResult:
				if !calls[b.ToolUseID] {
					return fmt.Errorf("tool_result %q answers a call outside the range", b.ToolUseID)
				}
				answered[b.ToolUseID] = true
			}
		}
	}
	for id := range calls {
		if !answered[id] {
			return fmt.Errorf("tool_use %q is not answered inside the range", id)
		}
	}
	return nil
}

// overlaps reports whether the ranges of a and b share a message.
func overlaps(a, b Elision) bool { return a.From < b.To && b.From < a.To }

// byFrom orders elisions by From.
func byFrom(a, b Elision) int { return cmp.Compare(a.From, b.From) }
