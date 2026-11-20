package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
)

// responsesReasoningIDKey is the ProviderData key under which a reasoning
// item's id is stored; the id must be replayed beside its encrypted content.
const responsesReasoningIDKey = "responses.reasoning_id"

// Responses stream event types.
const (
	respEvOutputItemAdded    = "response.output_item.added"
	respEvOutputItemDone     = "response.output_item.done"
	respEvTextDelta          = "response.output_text.delta"
	respEvReasoningDelta     = "response.reasoning_summary_text.delta"
	respEvReasoningText      = "response.reasoning_text.delta"
	respEvArgumentsDelta     = "response.function_call_arguments.delta"
	respEvCompleted          = "response.completed"
	respEvIncomplete         = "response.incomplete"
	respEvFailed             = "response.failed"
	respEvError              = "error"
	responsesMaxTokensReason = "max_output_tokens"
)

// responsesProvider speaks the OpenAI Responses API.
type responsesProvider struct {
	cfg *Config
}

// newResponses returns an adapter for cfg. It keeps its own copy of the
// configuration, so later changes to the caller's value have no effect.
func newResponses(cfg Config) (*responsesProvider, error) {
	if err := cfg.Prepare(); err != nil {
		return nil, err
	}
	return &responsesProvider{cfg: &cfg}, nil
}

// configuredModel implements modelReporter.
func (p *responsesProvider) configuredModel() string { return p.cfg.Model }

// Complete implements Provider by draining a stream.
func (p *responsesProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	return Collect(ctx, p, req)
}

// Stream implements Provider. Retries cover only establishing the response;
// a stream that breaks afterwards surfaces as an error and is not replayed.
func (p *responsesProvider) Stream(ctx context.Context, req Request) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		body, err := json.Marshal(p.buildRequest(req))
		if err != nil {
			yield(StreamEvent{}, fmt.Errorf("llm: encode responses request: %w", err))
			return
		}
		resp, err := p.send(ctx, body)
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		// Closed on every exit, including the consumer breaking out of the
		// loop, which releases the connection promptly.
		defer resp.Body.Close()
		emit := func(ev StreamEvent) bool { return yield(ev, nil) }
		if err := streamResponsesEvents(ctx, resp.Body, emit); err != nil {
			yield(StreamEvent{}, err)
		}
	}
}

func (p *responsesProvider) send(ctx context.Context, body []byte) (*http.Response, error) {
	return p.cfg.Do(ctx, func() (*http.Request, error) {
		url := strings.TrimRight(p.cfg.BaseURL, "/") + "/responses"
		httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		p.setHeaders(httpReq)
		return httpReq, nil
	})
}

func (p *responsesProvider) setHeaders(r *http.Request) {
	// Local servers often need no key, so the header is only sent with one.
	if p.cfg.APIKey != "" {
		r.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	r.Header.Set("content-type", "application/json")
	r.Header.Set("accept", "text/event-stream")
	for k, v := range p.cfg.Headers {
		r.Header.Set(k, v)
	}
}

// responsesEvent is the union of the stream event fields the adapter reads.
type responsesEvent struct {
	Type        string `json:"type"`
	OutputIndex int    `json:"output_index"`
	Delta       string `json:"delta"`
	Item        struct {
		Type             string `json:"type"`
		ID               string `json:"id"`
		CallID           string `json:"call_id"`
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		EncryptedContent string `json:"encrypted_content"`
	} `json:"item"`
	Response struct {
		Usage *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

// streamResponsesEvents translates the SSE body into events passed to emit.
// A body that ends without a terminal event is reported as an error so
// truncated replies are never mistaken for complete ones. It returns nil
// without further reading once emit reports the consumer is gone.
func streamResponsesEvents(ctx context.Context, body io.Reader, emit func(StreamEvent) bool) error {
	state := &responsesStreamState{argumentsSeen: make(map[int]bool), startedTools: make(map[int]bool)}
	if !emit(StreamEvent{Type: EvMessageStart}) {
		return nil
	}
	for sse, err := range readSSE(body) {
		if err != nil {
			return streamReadError(ctx, err)
		}
		events, done, err := state.translate(sse.Data)
		for _, ev := range events {
			if !emit(ev) {
				return nil
			}
		}
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return errors.New("llm: responses stream ended before a terminal event")
}

// responsesStreamState carries what must survive between events.
type responsesStreamState struct {
	// argumentsSeen records tool calls whose arguments streamed as deltas, so
	// the full text on the finished item is not applied twice.
	argumentsSeen map[int]bool
	startedTools  map[int]bool
	sawToolCall   bool
}

// translate maps one event to stream events. done is true once the closing
// events have been produced.
func (s *responsesStreamState) translate(data string) (events []StreamEvent, done bool, err error) {
	var ev responsesEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, false, fmt.Errorf("llm: decode responses event: %w", err)
	}
	index := ev.OutputIndex
	switch ev.Type {
	case respEvTextDelta:
		return []StreamEvent{{Type: EvTextDelta, Index: index, Text: ev.Delta}}, false, nil
	case respEvReasoningDelta, respEvReasoningText:
		return []StreamEvent{{Type: EvThinkingDelta, Index: index, Text: ev.Delta}}, false, nil
	case respEvArgumentsDelta:
		s.argumentsSeen[index] = true
		return []StreamEvent{{Type: EvToolInputDelta, Index: index, Text: ev.Delta}}, false, nil
	case respEvOutputItemAdded:
		return s.itemAdded(ev), false, nil
	case respEvOutputItemDone:
		return s.itemDone(ev), false, nil
	case respEvCompleted, respEvIncomplete:
		return s.closingEvents(ev), true, nil
	case respEvFailed, respEvError:
		// Status 0: the HTTP exchange itself succeeded.
		return nil, false, &APIError{Body: data}
	}
	return nil, false, nil
}

func (s *responsesStreamState) itemAdded(ev responsesEvent) []StreamEvent {
	if ev.Item.Type != responsesFunctionCallType {
		return nil
	}
	return s.startTool(ev)
}

// startTool records the call and announces it once per output index.
func (s *responsesStreamState) startTool(ev responsesEvent) []StreamEvent {
	s.sawToolCall = true
	if s.startedTools[ev.OutputIndex] {
		return nil
	}
	s.startedTools[ev.OutputIndex] = true
	return []StreamEvent{{Type: EvToolStart, Index: ev.OutputIndex, ToolID: ev.Item.CallID, ToolName: ev.Item.Name}}
}

func (s *responsesStreamState) itemDone(ev responsesEvent) []StreamEvent {
	index := ev.OutputIndex
	switch ev.Item.Type {
	case responsesReasoningType:
		return []StreamEvent{
			{Type: EvSignature, Index: index, Value: ev.Item.EncryptedContent},
			{Type: EvProviderData, Index: index, Key: responsesReasoningIDKey, Value: ev.Item.ID},
			formatStampEvent(index, FormatOpenAIResponses),
		}
	case responsesFunctionCallType:
		// Some servers send the call only on the finished item, arguments
		// included.
		events := s.startTool(ev)
		if !s.argumentsSeen[index] && ev.Item.Arguments != "" {
			events = append(events, StreamEvent{Type: EvToolInputDelta, Index: index, Text: ev.Item.Arguments})
		}
		return events
	}
	return nil
}

// closingEvents emits the final usage and stop reason for a terminal event.
func (s *responsesStreamState) closingEvents(ev responsesEvent) []StreamEvent {
	final := StreamEvent{Type: EvMessageDelta, StopReason: s.stopReason(ev)}
	if u := ev.Response.Usage; u != nil {
		cached := u.InputTokensDetails.CachedTokens
		final.Usage = Usage{Input: max(u.InputTokens-cached, 0), Output: u.OutputTokens, CacheRead: cached}
	}
	return []StreamEvent{final, {Type: EvMessageStop}}
}

func (s *responsesStreamState) stopReason(ev responsesEvent) string {
	if ev.Type == respEvIncomplete {
		if reason := ev.Response.IncompleteDetails.Reason; reason != responsesMaxTokensReason {
			return reason
		}
		return StopMaxTokens
	}
	if s.sawToolCall {
		return StopToolUse
	}
	return StopEndTurn
}
