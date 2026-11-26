package guard

import (
	"slices"

	"github.com/aswin-kevin/orchestrix/state"
)

// Verdict is the outcome of one check on one target.
type Verdict struct {
	// Target is "input" for the run's input, "tool:<name>" for a tool call.
	Target string `json:"target"`
	// Check is the name of the check.
	Check string `json:"check"`
	// Yes is the decider's probability that the check's answer is yes.
	Yes float64 `json:"yes"`
	// Blocked reports that this check stopped the input or the call, by its
	// answer or, failing closed, because it could not be completed.
	Blocked bool `json:"blocked"`
	// Refused reports that the decider declined to answer the check.
	Refused bool `json:"refused,omitempty"`
	// Error is why the check could not be completed, if it could not.
	Error string `json:"error,omitempty"`
}

// verdictsKey holds the verdicts of the current run, oldest first. Read it
// with Verdicts.
var verdictsKey = state.NewKey[[]Verdict]("guard.verdicts", state.Private[[]Verdict]())

// Verdicts returns the verdicts recorded in s by the current run.
func Verdicts(s *state.State) []Verdict {
	v, _ := verdictsKey.Get(s)
	return v
}

// record appends vs to the verdicts in s. Tool calls may run in parallel, so
// the append is atomic, and it never writes into a slice a reader holds.
func record(s *state.State, vs []Verdict) {
	verdictsKey.Update(s, func(cur []Verdict) []Verdict {
		return append(slices.Clip(cur), vs...)
	})
}
