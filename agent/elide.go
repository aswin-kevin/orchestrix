package agent

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
// user message holding Text. State itself is never changed.
type Elision struct {
	// ID names the elision, unique within a State; never empty once
	// recorded by AddElision.
	ID string `json:"id"`
	// From is the index of the first State message replaced; zero is the
	// first message.
	From int `json:"from"`
	// To is the index just past the last State message replaced; it is
	// always greater than From.
	To int `json:"to"`
	// Text is the content of the user message standing for the range;
	// never empty once recorded by AddElision.
	Text string `json:"text"`
}

// ElisionsKey holds a run's elisions, sorted by From and non-overlapping.
// Private (a subagent's history is its own); State.Fork copies it. Unset
// means no elisions.
//
// The indices stay valid because messages are only appended by the loop,
// and a resume inserts results right after an assistant message whose calls
// are unanswered, which no self-contained elision can contain; elisions
// lying after that insertion point (over messages a hook appended) are
// shifted by the number of messages inserted. A host that replaces State's
// messages should clear the key. ElidedView re-checks every elision against
// the current messages and skips one that no longer fits, so a stale key can
// make the view less compact but never invalid.
var ElisionsKey = state.NewKey[[]Elision]("agent.elisions", state.Private[[]Elision]())

// ErrInvalidElision is wrapped by the error AddElision returns for an
// elision it refuses.
var ErrInvalidElision = errors.New("agent: invalid elision")

// elisionsMu serializes the read-check-write of AddElision, RemoveElision
// and shiftElisions, so concurrent callers cannot record overlapping ranges.
var elisionsMu sync.Mutex

// AddElision records e after checking it against s: non-empty unique ID,
// non-empty Text, 0 <= From < To <= len(messages), no overlap, and the range
// is self-contained: every tool_use inside it is answered inside it and no
// tool_result inside it answers a call outside it. Otherwise an error
// wrapping ErrInvalidElision, and s is unchanged.
func AddElision(s *state.State, e Elision) error {
	elisionsMu.Lock()
	defer elisionsMu.Unlock()
	if err := checkElision(s.Messages(), e); err != nil {
		return fmt.Errorf("%w %q: %v", ErrInvalidElision, e.ID, err)
	}
	current, _ := ElisionsKey.Get(s)
	for _, other := range current {
		switch {
		case other.ID == e.ID:
			return fmt.Errorf("%w %q: duplicate id", ErrInvalidElision, e.ID)
		case overlaps(other, e):
			return fmt.Errorf("%w %q: [%d, %d) overlaps %q", ErrInvalidElision, e.ID, e.From, e.To, other.ID)
		}
	}
	updated := append(slices.Clone(current), e)
	slices.SortStableFunc(updated, byFrom)
	ElisionsKey.Set(s, updated)
	return nil
}

// RemoveElision deletes the elision with id; false when there is none. The
// key is cleared when its last elision goes.
func RemoveElision(s *state.State, id string) bool {
	elisionsMu.Lock()
	defer elisionsMu.Unlock()
	current, _ := ElisionsKey.Get(s)
	i := slices.IndexFunc(current, func(e Elision) bool { return e.ID == id })
	if i < 0 {
		return false
	}
	updated := slices.Delete(slices.Clone(current), i, i+1)
	if len(updated) == 0 {
		ElisionsKey.Delete(s)
	} else {
		ElisionsKey.Set(s, updated)
	}
	return true
}

// ElidedView is View(s.Messages()[from:]) with every elision lying wholly
// within [from, len(messages)) replaced by llm.UserText(e.Text). Elisions that
// start before from or end past the history are ignored. from is clamped to
// [0, len].
//
// Elisions that no longer fit the messages (empty ID or Text, an empty
// range, a range that is not self-contained, one overlapping an earlier
// elision) are ignored too: they appear only when the key was set directly
// or State was replaced, and applying them could make the request invalid.
func ElidedView(s *state.State, from int) []llm.Message {
	msgs := s.Messages()
	from = min(max(from, 0), len(msgs))
	applied := appliedElisions(s, msgs, from)
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
	return View(out)
}

// AppliedElisions returns the elisions ElidedView(s, from) applies, sorted by
// From. Middleware that maps the view back to State positions, such as one
// counting the assistant messages the view still holds, uses it.
func AppliedElisions(s *state.State, from int) []Elision {
	msgs := s.Messages()
	return appliedElisions(s, msgs, min(max(from, 0), len(msgs)))
}

// appliedElisions returns the elisions of s that fit msgs within
// [from, len(msgs)), sorted by From, skipping any that overlaps one kept
// before it.
func appliedElisions(s *state.State, msgs []llm.Message, from int) []Elision {
	all, _ := ElisionsKey.Get(s)
	if len(all) == 0 {
		return nil
	}
	sorted := slices.Clone(all)
	slices.SortStableFunc(sorted, byFrom)
	var applied []Elision
	for _, e := range sorted {
		if e.From < from || checkElision(msgs, e) != nil {
			continue
		}
		if len(applied) > 0 && overlaps(applied[len(applied)-1], e) {
			continue
		}
		applied = append(applied, e)
	}
	return applied
}

// shiftElisions moves the elisions of s past n messages inserted at index
// at: one starting at or after at moves whole, and one spanning at grows to
// include the inserted messages.
func shiftElisions(s *state.State, at, n int) {
	elisionsMu.Lock()
	defer elisionsMu.Unlock()
	current, _ := ElisionsKey.Get(s)
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
	ElisionsKey.Set(s, updated)
}

// checkElision reports why e does not fit msgs, or nil when it does; see
// AddElision. Uniqueness and overlap with other elisions are checked by the
// callers.
func checkElision(msgs []llm.Message, e Elision) error {
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
