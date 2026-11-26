package llmtest

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/aswin-kevin/orchestrix/llm"
)

// DecisionTurn is one scripted decision reply.
type DecisionTurn struct {
	// Model is the model name reported in the response.
	Model string
	// Answers are the answers returned, keyed by question name.
	Answers map[string]llm.Answer
	// Usage is the token usage the turn reports.
	Usage llm.Usage
	// Err, when set, is returned as is instead of a response.
	Err error
}

// ScriptedDecider is an llm.Decider that replays its turns in order, one per
// request. It is safe for concurrent use.
type ScriptedDecider struct {
	mu    sync.Mutex
	turns []DecisionTurn
	next  int

	// Requests holds a copy of every request received, in order. Read it only
	// after the calls under test have returned.
	Requests []llm.DecisionRequest
}

// NewDecider returns a ScriptedDecider that answers successive requests with
// turns.
func NewDecider(turns ...DecisionTurn) *ScriptedDecider {
	return &ScriptedDecider{turns: turns}
}

// Calls reports how many requests the decider has received.
func (d *ScriptedDecider) Calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.Requests)
}

// Decide records req and replies with the next scripted turn. When ctx is
// already done it returns ctx.Err() without recording req or consuming a
// turn. Once every turn is used it returns ErrExhausted.
func (d *ScriptedDecider) Decide(ctx context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Requests = append(d.Requests, cloneDecisionRequest(req))
	if d.next >= len(d.turns) {
		return nil, ErrExhausted
	}
	turn := d.turns[d.next]
	d.next++
	if turn.Err != nil {
		return nil, turn.Err
	}
	return &llm.DecisionResponse{Model: turn.Model, Answers: maps.Clone(turn.Answers), Usage: turn.Usage}, nil
}

// cloneDecisionRequest copies the slices of req that callers commonly mutate.
func cloneDecisionRequest(req llm.DecisionRequest) llm.DecisionRequest {
	req.Input = cloneBlocks(req.Input)
	req.Questions = slices.Clone(req.Questions)
	for i, q := range req.Questions {
		req.Questions[i].Choices = slices.Clone(q.Choices)
	}
	return req
}
