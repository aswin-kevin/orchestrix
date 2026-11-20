// Package llmtest provides a scripted llm.Provider for tests, so agent loops
// can be exercised deterministically without a network.
package llmtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"sync"

	"github.com/aswin-kevin/orchestrix/llm"
)

// emptyInput is the tool input streamed for a call that has none.
const emptyInput = "{}"

// errExhausted is returned when a request arrives after every turn was used.
var errExhausted = errors.New("llmtest: script exhausted")

// Call is one scripted tool call.
type Call struct {
	// ID is the tool_use id of the call.
	ID string
	// Name is the name of the tool to call.
	Name string
	// Input is marshaled to JSON. When the turn sets RawToolInput it must be
	// a string and is streamed verbatim, which lets tests send malformed JSON.
	Input any
}

// Turn is one scripted model reply.
type Turn struct {
	// Text is the text the turn streams first.
	Text string
	// Thinking is the reasoning text the turn streams before its text.
	Thinking string
	// ToolCalls are emitted after the text, each as its own content block.
	ToolCalls []Call
	// StopReason defaults to tool_use when ToolCalls is set, else end_turn.
	StopReason string
	// Usage is the token usage the turn reports.
	Usage llm.Usage
	// Err, when set, is returned instead of any events.
	Err error
	// RawToolInput makes every Call.Input a string used without marshaling.
	RawToolInput bool
}

// Scripted is a Provider that replays its turns in order, one per request.
type Scripted struct {
	mu    sync.Mutex
	turns []Turn
	next  int

	// Requests holds a copy of every request received, in order. Read it only
	// after the calls under test have returned.
	Requests []llm.Request
}

// New returns a Scripted provider that answers successive requests with turns.
func New(turns ...Turn) *Scripted {
	return &Scripted{turns: turns}
}

// Calls reports how many requests the provider has received.
func (s *Scripted) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Requests)
}

// Stream records req and streams the next scripted turn.
func (s *Scripted) Stream(_ context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		turn, err := s.take(req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		events, err := turn.events()
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Complete records req and returns the next scripted turn as a Response.
func (s *Scripted) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, s, req)
}

// take records req and returns the turn that answers it.
func (s *Scripted) take(req llm.Request) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, cloneRequest(req))
	if s.next >= len(s.turns) {
		return Turn{}, errExhausted
	}
	turn := s.turns[s.next]
	s.next++
	return turn, turn.Err
}

// events renders the turn as the sequence a real provider would stream:
// thinking, then text, then each tool call, every block at its own index.
func (t Turn) events() ([]llm.StreamEvent, error) {
	evs := []llm.StreamEvent{{
		Type: llm.EvMessageStart,
		Usage: llm.Usage{
			Input:      t.Usage.Input,
			CacheRead:  t.Usage.CacheRead,
			CacheWrite: t.Usage.CacheWrite,
		},
	}}
	index := 0
	if t.Thinking != "" {
		evs = append(evs, llm.StreamEvent{Type: llm.EvThinkingDelta, Index: index, Text: t.Thinking})
		index++
	}
	if t.Text != "" {
		evs = append(evs, llm.StreamEvent{Type: llm.EvTextDelta, Index: index, Text: t.Text})
		index++
	}
	for _, c := range t.ToolCalls {
		input, err := t.toolInput(c)
		if err != nil {
			return nil, err
		}
		evs = append(evs,
			llm.StreamEvent{Type: llm.EvToolStart, Index: index, ToolID: c.ID, ToolName: c.Name},
			llm.StreamEvent{Type: llm.EvToolInputDelta, Index: index, Text: input},
		)
		index++
	}
	return append(evs,
		llm.StreamEvent{
			Type:       llm.EvMessageDelta,
			StopReason: t.stopReason(),
			Usage:      llm.Usage{Output: t.Usage.Output},
		},
		llm.StreamEvent{Type: llm.EvMessageStop},
	), nil
}

// toolInput returns the JSON text streamed as the input of call c.
func (t Turn) toolInput(c Call) (string, error) {
	if c.Input == nil {
		return emptyInput, nil
	}
	if t.RawToolInput {
		raw, ok := c.Input.(string)
		if !ok {
			return "", fmt.Errorf("llmtest: RawToolInput needs string input for %q, got %T", c.Name, c.Input)
		}
		return raw, nil
	}
	b, err := json.Marshal(c.Input)
	if err != nil {
		return "", fmt.Errorf("llmtest: marshal input for %q: %w", c.Name, err)
	}
	return string(b), nil
}

func (t Turn) stopReason() string {
	switch {
	case t.StopReason != "":
		return t.StopReason
	case len(t.ToolCalls) > 0:
		return llm.StopToolUse
	default:
		return llm.StopEndTurn
	}
}

// cloneRequest copies the parts of req that callers commonly mutate between
// calls (messages, system prompt, tools, stop sequences) so a recording stays
// as it was when received.
func cloneRequest(req llm.Request) llm.Request {
	req.System = slices.Clone(req.System)
	req.Stop = slices.Clone(req.Stop)
	req.Tools = slices.Clone(req.Tools)
	req.Messages = cloneMessages(req.Messages)
	return req
}

func cloneMessages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		m.Content = cloneBlocks(m.Content)
		m.Meta = maps.Clone(m.Meta)
		out[i] = m
	}
	return out
}

func cloneBlocks(blocks []llm.Block) []llm.Block {
	if blocks == nil {
		return nil
	}
	out := make([]llm.Block, len(blocks))
	for i, b := range blocks {
		b.Input = slices.Clone(b.Input)
		b.Content = cloneBlocks(b.Content)
		b.ProviderData = maps.Clone(b.ProviderData)
		if b.Source != nil {
			src := *b.Source
			b.Source = &src
		}
		out[i] = b
	}
	return out
}
