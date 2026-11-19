package rubric

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
)

// DefaultPassThreshold is the probability of yes at which a criterion counts
// as met when DecisionGrader.Threshold is zero.
const DefaultPassThreshold = 0.5

// DecisionGrader grades by asking a decision model one yes/no question per
// criterion, in a single call. A criterion is met when the probability of yes
// reaches Threshold, and the answer passes when every criterion is met.
type DecisionGrader struct {
	// Decider answers the questions; required.
	Decider llm.Decider
	// Model is sent as the request model; "" means the Decider's default.
	Model string
	// Threshold is the probability of yes a criterion needs; 0 means
	// DefaultPassThreshold.
	Threshold float64
	// Timeout bounds the grading call; 0 means DefaultGradeTimeout. A call
	// that runs out fails, and the run completes (grading fails open).
	Timeout time.Duration
}

// Grade implements Grader. A refused or missing answer is an error, so the
// middleware lets the run through rather than failing the answer.
func (g DecisionGrader) Grade(ctx context.Context, req GradeRequest) (Grade, error) {
	if g.Decider == nil {
		return Grade{}, errors.New("rubric: DecisionGrader needs a Decider")
	}
	threshold := g.Threshold
	if threshold <= 0 {
		threshold = DefaultPassThreshold
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultGradeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	questions := make([]llm.Question, len(req.Criteria))
	for i, c := range req.Criteria {
		questions[i] = llm.Question{
			Name:         criterionName(i),
			Type:         llm.YesNo,
			Instructions: "Does the answer inside <answer> fully meet this criterion? " + c,
		}
	}
	resp, err := g.Decider.Decide(ctx, llm.DecisionRequest{
		Model:     g.Model,
		Input:     []llm.Block{llm.TextBlock(gradingMessage(req))},
		Questions: questions,
	})
	if err != nil {
		return Grade{}, err
	}
	if resp == nil {
		return Grade{}, errors.New("rubric: decider returned no response")
	}
	var failed strings.Builder
	for i, c := range req.Criteria {
		a, ok := resp.Answers[criterionName(i)]
		switch {
		case !ok:
			return Grade{}, fmt.Errorf("rubric: decider gave no answer for criterion %d", i+1)
		case a.Refused:
			return Grade{}, fmt.Errorf("rubric: decider refused criterion %d", i+1)
		case a.Yes < threshold:
			fmt.Fprintf(&failed, "%d. %s\n", i+1, c)
		}
	}
	if failed.Len() == 0 {
		return Grade{Pass: true}, nil
	}
	return Grade{Feedback: "Criteria not met:\n" + failed.String()}, nil
}

// criterionName is the question name of the i-th (0-based) criterion.
func criterionName(i int) string { return fmt.Sprintf("c%d", i+1) }
