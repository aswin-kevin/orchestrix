package elide

import (
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/internal/elision"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// Elision replaces State messages [From, To) in the request view with one
// user message holding Text. State itself is never changed.
type Elision struct {
	// ID names the elision, unique within a State; never empty once
	// recorded by Add.
	ID string `json:"id"`
	// From is the index of the first State message replaced; zero is the
	// first message.
	From int `json:"from"`
	// To is the index just past the last State message replaced; it is
	// always greater than From.
	To int `json:"to"`
	// Text is the content of the user message standing for the range;
	// never empty once recorded by Add.
	Text string `json:"text"`
}

// ErrInvalid is wrapped by the error Add returns for an elision it refuses.
var ErrInvalid = elision.ErrInvalid

// Add records e after checking it against s: non-empty unique ID, non-empty
// Text, 0 <= From < To <= len(messages), no overlap, and the range is
// self-contained: every tool_use inside it is answered inside it and no
// tool_result inside it answers a call outside it. Otherwise an error
// wrapping ErrInvalid, and s is unchanged. It is safe to call concurrently.
func Add(s *state.State, e Elision) error {
	return elision.Add(s, elision.Elision(e))
}

// Remove deletes the elision with id; false when there is none. The stored
// elisions are cleared when the last one goes.
func Remove(s *state.State, id string) bool {
	return elision.Remove(s, id)
}

// All returns a copy of the elisions stored in s, as stored (Add keeps them
// sorted by From); nil when there are none. Changing the result does not
// change s.
func All(s *state.State) []Elision {
	return fromInternal(elision.All(s))
}

// Clear deletes every elision of s. Call it whenever State's messages are
// replaced.
func Clear(s *state.State) {
	elision.Clear(s)
}

// Applied returns the elisions View(s, from) applies, sorted by From.
// Middleware that maps the view back to State positions, such as one
// counting the assistant messages the view still holds, uses it.
func Applied(s *state.State, from int) []Elision {
	return fromInternal(elision.Applied(s, from))
}

// View is agent.View(s.Messages()[from:]) with every elision lying wholly
// within [from, len(messages)) replaced by llm.UserText(e.Text): the
// messages the agent loop sends for s when from is zero. Elisions that start
// before from or end past the history are ignored. from is clamped to
// [0, len].
//
// Elisions that no longer fit the messages (empty ID or Text, an empty
// range, a range that is not self-contained, one overlapping an earlier
// elision) are ignored too: they appear only when State was replaced or
// restored from altered data, and applying them could make the request
// invalid.
func View(s *state.State, from int) []llm.Message {
	return agent.View(elision.View(s, from))
}

// Check reports why e does not fit msgs, or nil when it does: the checks of
// Add on ID, Text, range and self-containment. It does not check e against
// other elisions (uniqueness, overlap). Middleware that chooses ranges to
// elide uses it to propose only ranges Add accepts.
func Check(msgs []llm.Message, e Elision) error {
	return elision.Check(msgs, elision.Elision(e))
}

// fromInternal converts elisions of the internal package.
func fromInternal(in []elision.Elision) []Elision {
	if in == nil {
		return nil
	}
	out := make([]Elision, len(in))
	for i, e := range in {
		out[i] = Elision(e)
	}
	return out
}
