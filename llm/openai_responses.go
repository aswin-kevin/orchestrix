package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
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
	cfg Config
	t   *Transport
}

// newResponses returns an adapter for cfg. It keeps its own copy of the
// configuration, so later changes to the caller's value have no effect.
func newResponses(cfg Config) (*responsesProvider, error) {
	t, err := NewTransport(cfg)
	if err != nil {
		return nil, err
	}
	return &responsesProvider{cfg: t.Config(), t: t}, nil
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
		body, err := encodeBody(req, p.buildRequest(req))
		if err != nil {
			yield(StreamEvent{}, fmt.Errorf("llm: encode responses request: %w", err))
			return
		}
		// The body is closed on every exit, including the consumer breaking
		// out of the loop, which releases the connection promptly.
		emit := func(ev StreamEvent) bool { return yield(ev, nil) }
		if err := streamResponsesEvents(p.t, p.t.Stream(ctx, p.request(body)), emit); err != nil {
			yield(StreamEvent{}, err)
		}
	}
}

// request returns the func that builds the HTTP request for body, once per
// attempt.
func (p *responsesProvider) request(body []byte) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		target, err := endpoint(p.cfg.BaseURL, "responses")
		if err != nil {
			return nil, err
		}
		httpReq, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		p.setHeaders(httpReq)
		return httpReq, nil
	}
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
	ItemID      string `json:"item_id"`
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
		ID    string `json:"id"`
		Model string `json:"model"`
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

// streamResponsesEvents translates the SSE events into events passed to
// emit, starting with a message start sent when the first event arrives (or
// when the body ends with no events; a read error before any event sends
// none). A body that ends without a terminal event is reported as an error so
// truncated replies are never mistaken for complete ones. It returns nil
// without further reading once emit reports the consumer is gone.
func streamResponsesEvents(t *Transport, events iter.Seq2[SSEEvent, error], emit func(StreamEvent) bool) error {
	state := &responsesStreamState{
		t:             t,
		argumentsSeen: make(map[int]bool),
		startedTools:  make(map[int]bool),
		blocks:        make(map[int]int),
		byID:          make(map[string]int),
		items:         make(map[int]responsesItem),
	}
	started := false
	for sse, err := range events {
		if err != nil {
			return err
		}
		if !started {
			started = true
			if !emit(StreamEvent{Type: EventMessageStart}) {
				return nil
			}
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
	if !started && !emit(StreamEvent{Type: EventMessageStart}) {
		return nil
	}
	return errors.New("llm: responses stream ended before a terminal event")
}

// responsesStreamState carries what must survive between events.
type responsesStreamState struct {
	// t redacts credentials from errors reported in the stream.
	t *Transport
	// argumentsSeen records tool calls whose arguments streamed as deltas, so
	// the full text on the finished item is not applied twice.
	argumentsSeen map[int]bool
	startedTools  map[int]bool
	// Blocks are numbered by output item, not by output_index: the spec
	// gives every item its own output_index, but some servers reuse one for
	// a later item (a reasoning item after the message). blocks maps an
	// output_index to the block of the item last seen there, byID maps an
	// item id to its block, and items records each block's item.
	// argumentsSeen and startedTools are keyed by block index.
	blocks      map[int]int
	byID        map[string]int
	items       map[int]responsesItem
	next        int
	sawToolCall bool
	// model and id are the first the events reported (response.created
	// carries them), reported with the closing events.
	model string
	id    string
}

// translate maps one event to stream events. done is true once the closing
// events have been produced.
func (s *responsesStreamState) translate(data string) (events []StreamEvent, done bool, err error) {
	var ev responsesEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, false, fmt.Errorf("llm: decode responses event: %w", err)
	}
	s.model = firstNonEmpty(s.model, ev.Response.Model)
	s.id = firstNonEmpty(s.id, ev.Response.ID)
	switch ev.Type {
	case respEvTextDelta:
		return []StreamEvent{{Type: EventTextDelta, Index: s.blockFor(ev.OutputIndex, ev.ItemID, BlockText), Text: ev.Delta}}, false, nil
	case respEvReasoningDelta, respEvReasoningText:
		return []StreamEvent{{Type: EventThinkingDelta, Index: s.blockFor(ev.OutputIndex, ev.ItemID, BlockThinking), Text: ev.Delta}}, false, nil
	case respEvArgumentsDelta:
		index := s.blockFor(ev.OutputIndex, ev.ItemID, BlockToolUse)
		s.argumentsSeen[index] = true
		return []StreamEvent{{Type: EventToolInputDelta, Index: index, Text: ev.Delta}}, false, nil
	case respEvOutputItemAdded:
		return s.itemAdded(ev), false, nil
	case respEvOutputItemDone:
		return s.itemDone(ev), false, nil
	case respEvCompleted, respEvIncomplete:
		return s.closingEvents(ev), true, nil
	case respEvFailed, respEvError:
		// Status 0: the HTTP exchange itself succeeded.
		return nil, false, s.t.StreamError(data)
	}
	return nil, false, nil
}

// itemAdded opens the block of an announced item and starts a tool call.
func (s *responsesStreamState) itemAdded(ev responsesEvent) []StreamEvent {
	index := s.announce(ev)
	if ev.Item.Type != responsesFunctionCallType {
		return nil
	}
	return s.startTool(index, ev)
}

// responsesItem identifies the output item a block holds: its id (empty when
// the server sent none) and the block type its events fill ("" for an item
// type the adapter does not read, until an event of a known type claims it).
type responsesItem struct {
	id   string
	kind BlockType
}

// announce returns the block for an output_item.added event: the block of an
// item already seen under the same id, else a new block at the event's
// output_index.
func (s *responsesStreamState) announce(ev responsesEvent) int {
	kind := responsesItemKind(ev.Item.Type)
	if b, ok := s.known(ev.Item.ID, kind); ok {
		s.blocks[ev.OutputIndex] = b
		return b
	}
	return s.open(ev.OutputIndex, responsesItem{id: ev.Item.ID, kind: kind})
}

// blockFor returns the block for an event of type kind about the item with
// id at output index out. It finds the item by id when the server sent one,
// else takes the item last seen at out when it holds kind (or no type yet,
// or carries no id while the event names one), and otherwise opens a new
// block, so an event never lands in a block of another type.
func (s *responsesStreamState) blockFor(out int, id string, kind BlockType) int {
	if b, ok := s.known(id, kind); ok {
		return b
	}
	if b, ok := s.blocks[out]; ok {
		cur := s.items[b]
		if (cur.kind == kind || cur.kind == "") && (id == "" || cur.id == "") {
			cur.kind = kind
			if id != "" {
				cur.id = id
				s.byID[id] = b
			}
			s.items[b] = cur
			return b
		}
	}
	return s.open(out, responsesItem{id: id, kind: kind})
}

// known returns the block of the item with id when one was seen and holds
// kind (or no type yet, or kind is ""), filling in its type.
func (s *responsesStreamState) known(id string, kind BlockType) (int, bool) {
	if id == "" {
		return 0, false
	}
	b, ok := s.byID[id]
	if !ok {
		return 0, false
	}
	cur := s.items[b]
	switch {
	case kind == "" || cur.kind == kind:
	case cur.kind == "":
		cur.kind = kind
		s.items[b] = cur
	default:
		return 0, false
	}
	return b, true
}

// open numbers a new block for out and records its item.
func (s *responsesStreamState) open(out int, item responsesItem) int {
	b := s.next
	s.next++
	s.blocks[out] = b
	s.items[b] = item
	if item.id != "" {
		s.byID[item.id] = b
	}
	return b
}

// responsesItemKind returns the block type an output item's events fill, or
// "" for an item type the adapter does not read.
func responsesItemKind(itemType string) BlockType {
	switch itemType {
	case responsesMessageType:
		return BlockText
	case responsesReasoningType:
		return BlockThinking
	case responsesFunctionCallType:
		return BlockToolUse
	}
	return ""
}

// startTool records the call and announces it once per block.
func (s *responsesStreamState) startTool(index int, ev responsesEvent) []StreamEvent {
	s.sawToolCall = true
	if s.startedTools[index] {
		return nil
	}
	s.startedTools[index] = true
	return []StreamEvent{{Type: EventToolStart, Index: index, ToolID: ev.Item.CallID, ToolName: ev.Item.Name}}
}

// itemDone emits what only a finished item carries: a reasoning item's
// signature and id, and a tool call sent whole on its finished item. A
// finished message carries nothing its deltas did not.
func (s *responsesStreamState) itemDone(ev responsesEvent) []StreamEvent {
	if ev.Item.Type != responsesReasoningType && ev.Item.Type != responsesFunctionCallType {
		return nil
	}
	index := s.blockFor(ev.OutputIndex, ev.Item.ID, responsesItemKind(ev.Item.Type))
	switch ev.Item.Type {
	case responsesReasoningType:
		return []StreamEvent{
			{Type: EventSignature, Index: index, Value: ev.Item.EncryptedContent},
			{Type: EventProviderData, Index: index, Key: responsesReasoningIDKey, Value: ev.Item.ID},
			formatStampEvent(index, FormatOpenAIResponses),
		}
	case responsesFunctionCallType:
		// Some servers send the call only on the finished item, arguments
		// included.
		events := s.startTool(index, ev)
		if !s.argumentsSeen[index] && ev.Item.Arguments != "" {
			events = append(events, StreamEvent{Type: EventToolInputDelta, Index: index, Text: ev.Item.Arguments})
		}
		return events
	}
	return nil
}

// closingEvents emits the final usage and stop reason for a terminal event.
func (s *responsesStreamState) closingEvents(ev responsesEvent) []StreamEvent {
	final := StreamEvent{Type: EventMessageDelta, StopReason: s.stopReason(ev), Model: s.model, ResponseID: s.id}
	if u := ev.Response.Usage; u != nil {
		cached := u.InputTokensDetails.CachedTokens
		final.Usage = Usage{Input: max(u.InputTokens-cached, 0), Output: u.OutputTokens, CacheRead: cached}
	}
	return []StreamEvent{final, {Type: EventMessageStop}}
}

func (s *responsesStreamState) stopReason(ev responsesEvent) StopReason {
	if ev.Type == respEvIncomplete {
		if reason := ev.Response.IncompleteDetails.Reason; reason != responsesMaxTokensReason {
			return StopReason(reason)
		}
		return StopMaxTokens
	}
	if s.sawToolCall {
		return StopToolUse
	}
	return StopEndTurn
}
