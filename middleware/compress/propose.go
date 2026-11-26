package compress

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/internal/msgutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/state"
)

// span is a range [From, To) of State messages.
type span struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// covers reports whether i lies in r.
func (r span) covers(i int) bool { return r.From <= i && i < r.To }

// selectRange returns the range to propose: the earliest settled run of
// whole user turns at or after summarize's boundary, outside every elision
// and every range restored this run, ending at the next of those or at the
// newest KeepTurns turns, which are never proposed. The range is shortened
// to an earlier turn start, or moved to a later one, until it is
// self-contained (no call left unanswered inside it); it must reach
// MinRangeTokens. false when no range qualifies.
func (m *Middleware) selectRange(s *state.State) (span, bool) {
	msgs := s.Messages()
	from := boundary(s, len(msgs))
	starts := turnStarts(msgs, from)
	if len(starts) <= m.cfg.KeepTurns {
		return span{}, false
	}
	// The current turn is among the kept ones: the last turn start is.
	limit := starts[len(starts)-m.cfg.KeepTurns]
	candidates := starts[:len(starts)-m.cfg.KeepTurns]
	blocked := obstacles(s, from)
	chars := prefixChars(msgs)
	minChars := m.cfg.MinRangeTokens * summarize.CharsPerToken
	for i, start := range candidates {
		if slices.ContainsFunc(blocked, func(r span) bool { return r.covers(start) }) {
			continue
		}
		stop := limit
		for _, r := range blocked {
			if r.From > start && r.From < stop {
				stop = r.From
			}
		}
		// Ends to try, longest first: the stop, then each turn start
		// before it.
		ends := []int{stop}
		for j := len(candidates) - 1; j > i; j-- {
			if candidates[j] < stop {
				ends = append(ends, candidates[j])
			}
		}
		for _, end := range ends {
			if chars[end]-chars[start] < minChars {
				break // shorter ends are smaller still
			}
			if fits(msgs, start, end) {
				return span{From: start, To: end}, true
			}
		}
	}
	return span{}, false
}

// turnStarts returns the indices, at or after from, of the user messages
// that start a turn: neither tool results nor injected by a middleware.
func turnStarts(msgs []llm.Message, from int) []int {
	var starts []int
	for i := from; i < len(msgs); i++ {
		if msgutil.IsUserTurn(msgs[i]) {
			starts = append(starts, i)
		}
	}
	return starts
}

// obstacles returns the ranges a proposal must not touch: the elisions at or
// after from (those before it are dropped when the range is applied) and the
// ranges restored this run.
func obstacles(s *state.State, from int) []span {
	var out []span
	elisions := elide.All(s)
	for _, e := range elisions {
		if e.From >= from {
			out = append(out, span{From: e.From, To: e.To})
		}
	}
	restored, _ := restoredKey.Get(s)
	return append(out, restored...)
}

// prefixChars returns, for each i in [0, len(msgs)], the estimated size in
// characters of msgs[:i], measured as summarize measures it.
func prefixChars(msgs []llm.Message) []int {
	out := make([]int, len(msgs)+1)
	for i, msg := range msgs {
		out[i+1] = out[i] + len(summarize.Transcript([]llm.Message{msg}))
	}
	return out
}

// fits reports whether elide.Add would accept [start, end) of msgs:
// the range is self-contained (no call left unanswered inside it, no result
// answering a call outside it).
func fits(msgs []llm.Message, start, end int) bool {
	// ID and Text are placeholders: only the range is in question here.
	return elide.Check(msgs, elide.Elision{ID: "candidate", Text: "candidate", From: start, To: end}) == nil
}

// compressID matches the IDs this package issues.
var compressID = regexp.MustCompile(`^c([1-9][0-9]*)$`)

// nextID issues the next ID, "c<N>": N is one more than both the highest N
// issued in this State and the highest N among its elisions, so an ID is
// never reused, across runs either.
func nextID(s *state.State) string {
	n, _ := issuedKey.Get(s)
	elisions := elide.All(s)
	for _, e := range elisions {
		n = max(n, idNumber(e.ID))
	}
	n++
	issuedKey.Set(s, n)
	return fmt.Sprintf("c%d", n)
}

// noteIssued records id as issued, so nextID never reissues it once its
// elision is gone.
func noteIssued(s *state.State, id string) {
	if n := idNumber(id); n > 0 {
		issuedKey.Update(s, func(v int) int { return max(v, n) })
	}
}

// idNumber returns N of an ID "c<N>"; 0 for any other ID.
func idNumber(id string) int {
	m := compressID.FindStringSubmatch(id)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0 // out of range
	}
	return n
}
