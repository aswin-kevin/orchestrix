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

const (
	defaultAnthropicVersion = "2023-06-01"

	// minThinkingBudget is the smallest thinking budget the API accepts.
	minThinkingBudget = 1024

	// anthropicRedactedThinking is the wire type of a thinking block whose
	// reasoning the API returns encrypted.
	anthropicRedactedThinking BlockType = "redacted_thinking"
	// anthropicRedactedKey marks, in Block.ProviderData, a BlockThinking that
	// arrived as redacted thinking. Its encrypted data is kept in Signature,
	// the field that already holds a thinking block's opaque replay token.
	anthropicRedactedKey = "anthropic.redacted"
)

// anthropicProvider speaks the Anthropic Messages API.
type anthropicProvider struct {
	cfg *Config
}

// newAnthropic returns an adapter for cfg. It keeps its own copy of the
// configuration, so later changes to the caller's value have no effect.
func newAnthropic(cfg Config) (*anthropicProvider, error) {
	if err := cfg.Prepare(); err != nil {
		return nil, err
	}
	return &anthropicProvider{cfg: &cfg}, nil
}

// configuredModel implements modelReporter.
func (p *anthropicProvider) configuredModel() string { return p.cfg.Model }

// Wire types. They mirror the Messages API closely and exist only so that
// local-only fields (ProviderData, Meta) can never reach the network.

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        []anthropicSystem  `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
	OutputConfig  *anthropicOutput   `json:"output_config,omitempty"`
	Stream        bool               `json:"stream"`
}

type anthropicSystem struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl *anthropicCacheCt `json:"cache_control,omitempty"`
}

type anthropicCacheCt struct {
	Type string `json:"type"`
}

type anthropicMessage struct {
	Role    Role             `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      BlockType        `json:"type"`
	Text      string           `json:"text,omitempty"`
	Thinking  *string          `json:"thinking,omitempty"` // set on thinking blocks, even when empty
	Signature string           `json:"signature,omitempty"`
	Data      string           `json:"data,omitempty"` // redacted thinking
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     json.RawMessage  `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   []anthropicBlock `json:"content,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
	Source    *Source          `json:"source,omitempty"`

	CacheControl *anthropicCacheCt `json:"cache_control,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicOutput struct {
	Effort string `json:"effort"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) toUsage() Usage {
	return Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
}

// anthropicResponse is the non-streaming reply.
type anthropicResponse struct {
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      anthropicUsage   `json:"usage"`
}

// anthropicEvent is the superset of fields carried by streaming events.
type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Data string `json:"data"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
}

// Complete implements Provider with a non-streaming request.
func (p *anthropicProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	resp, err := p.send(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var wire anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("llm: decode anthropic response: %w", err)
	}
	return wire.toResponse(), nil
}

func (r anthropicResponse) toResponse() *Response {
	var content []Block
	for _, b := range r.Content {
		if block, ok := fromAnthropicBlock(b); ok {
			content = append(content, block)
		}
	}
	return &Response{
		Message:    Message{Role: RoleAssistant, Content: content},
		StopReason: r.StopReason,
		Usage:      r.Usage.toUsage(),
	}
}

// fromAnthropicBlock converts a reply block; kinds the model layer has no use
// for are skipped.
func fromAnthropicBlock(b anthropicBlock) (Block, bool) {
	switch b.Type {
	case BlockText:
		return Block{Type: BlockText, Text: b.Text}, true
	case BlockThinking:
		block := Block{Type: BlockThinking, Signature: b.Signature}
		if b.Thinking != nil {
			block.Thinking = *b.Thinking
		}
		stampFormat(&block, FormatAnthropic)
		return block, true
	case anthropicRedactedThinking:
		return redactedThinkingBlock(b.Data), true
	case BlockToolUse:
		return Block{Type: BlockToolUse, ID: b.ID, Name: b.Name, Input: toolInput(b.Input)}, true
	}
	return Block{}, false
}

// Stream implements Provider. Retries cover only establishing the response;
// a stream that breaks midway surfaces as an error and is not replayed, since
// the consumer has already seen part of it.
func (p *anthropicProvider) Stream(ctx context.Context, req Request) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		resp, err := p.send(ctx, req, true)
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		// Closing on every exit, including the consumer breaking out of the
		// loop, releases the connection promptly.
		defer resp.Body.Close()
		streamAnthropicEvents(ctx, resp.Body, yield)
	}
}

// streamAnthropicEvents translates the SSE body into stream events. A body
// that ends before message_stop is reported as an error so truncated replies
// are never mistaken for complete ones.
func streamAnthropicEvents(ctx context.Context, body io.Reader, yield func(StreamEvent, error) bool) {
	for sse, err := range readSSE(body) {
		if err != nil {
			yield(StreamEvent{}, streamReadError(ctx, err))
			return
		}
		events, done, err := translateAnthropicEvent(sse)
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		if done {
			return
		}
	}
	yield(StreamEvent{}, errors.New("llm: anthropic stream ended before message_stop"))
}

// translateAnthropicEvent maps one SSE event to zero or more stream events.
// done is true once the message is complete.
func translateAnthropicEvent(sse sseEvent) (events []StreamEvent, done bool, err error) {
	var ev anthropicEvent
	if err := json.Unmarshal([]byte(sse.Data), &ev); err != nil {
		return nil, false, fmt.Errorf("llm: decode anthropic event: %w", err)
	}
	switch ev.Type {
	case "message_start":
		return messageStartEvents(ev), false, nil
	case "content_block_start":
		return blockStartEvents(ev), false, nil
	case "content_block_delta":
		return blockDeltaEvents(ev), false, nil
	case "message_delta":
		return messageDeltaEvents(ev), false, nil
	case "message_stop":
		return []StreamEvent{{Type: EvMessageStop}}, true, nil
	case "error":
		// Status 0: the HTTP exchange itself succeeded.
		return nil, false, &APIError{Body: sse.Data}
	}
	// ping and unknown event types carry nothing we need.
	return nil, false, nil
}

func messageStartEvents(ev anthropicEvent) []StreamEvent {
	if ev.Message == nil {
		return []StreamEvent{{Type: EvMessageStart}}
	}
	usage := ev.Message.Usage.toUsage()
	// message_start reports a provisional output count and message_delta then
	// reports the running total, so counting both would double-count.
	usage.Output = 0
	return []StreamEvent{{Type: EvMessageStart, Usage: usage}}
}

func blockStartEvents(ev anthropicEvent) []StreamEvent {
	if ev.ContentBlock == nil {
		return nil
	}
	switch BlockType(ev.ContentBlock.Type) {
	case BlockToolUse:
		return []StreamEvent{{
			Type: EvToolStart, Index: ev.Index,
			ToolID: ev.ContentBlock.ID, ToolName: ev.ContentBlock.Name,
		}}
	case anthropicRedactedThinking:
		// Redacted thinking arrives whole here and gets no deltas.
		return []StreamEvent{
			{Type: EvSignature, Index: ev.Index, Value: ev.ContentBlock.Data},
			{Type: EvProviderData, Index: ev.Index, Key: anthropicRedactedKey, Value: "true"},
			formatStampEvent(ev.Index, FormatAnthropic),
		}
	}
	// Text and thinking blocks are created by their first delta.
	return nil
}

func blockDeltaEvents(ev anthropicEvent) []StreamEvent {
	if ev.Delta == nil {
		return nil
	}
	d := ev.Delta
	switch d.Type {
	case "text_delta":
		return []StreamEvent{{Type: EvTextDelta, Index: ev.Index, Text: d.Text}}
	case "thinking_delta":
		return []StreamEvent{{Type: EvThinkingDelta, Index: ev.Index, Text: d.Thinking}}
	case "signature_delta":
		// Every thinking block ends with its signature, which also makes the
		// block exist even when no thinking text streamed, so it is stamped
		// here.
		return []StreamEvent{
			{Type: EvSignature, Index: ev.Index, Value: d.Signature},
			formatStampEvent(ev.Index, FormatAnthropic),
		}
	case "input_json_delta":
		return []StreamEvent{{Type: EvToolInputDelta, Index: ev.Index, Text: d.PartialJSON}}
	}
	return nil
}

func messageDeltaEvents(ev anthropicEvent) []StreamEvent {
	out := StreamEvent{Type: EvMessageDelta}
	if ev.Delta != nil {
		out.StopReason = ev.Delta.StopReason
	}
	if ev.Usage != nil {
		out.Usage = ev.Usage.toUsage()
	}
	return []StreamEvent{out}
}

// send builds and issues the HTTP request, returning the 2xx response.
func (p *anthropicProvider) send(ctx context.Context, req Request, stream bool) (*http.Response, error) {
	body, err := json.Marshal(p.buildRequest(req, stream))
	if err != nil {
		return nil, fmt.Errorf("llm: encode anthropic request: %w", err)
	}
	return p.cfg.Do(ctx, func() (*http.Request, error) {
		httpReq, err := http.NewRequest(http.MethodPost, p.messagesURL(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		p.setHeaders(httpReq, stream)
		return httpReq, nil
	})
}

func (p *anthropicProvider) messagesURL() string {
	base := strings.TrimRight(p.cfg.BaseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

func (p *anthropicProvider) setHeaders(r *http.Request, stream bool) {
	version := p.cfg.APIVersion
	if version == "" {
		version = defaultAnthropicVersion
	}
	r.Header.Set("x-api-key", p.cfg.APIKey)
	r.Header.Set("anthropic-version", version)
	r.Header.Set("content-type", "application/json")
	if stream {
		r.Header.Set("accept", "text/event-stream")
	}
	for k, v := range p.cfg.Headers {
		r.Header.Set(k, v)
	}
}

func (p *anthropicProvider) buildRequest(req Request, stream bool) anthropicRequest {
	thinking := firstNonEmpty(req.Thinking, p.cfg.Thinking)
	thinkingCfg, maxTokens := toAnthropicThinking(thinking,
		firstNonZero(req.MaxTokens, p.cfg.DefaultMaxTokens, DefaultMaxTokens))
	effort := firstNonEmpty(req.ReasoningEffort, p.cfg.ReasoningEffort)

	wire := anthropicRequest{
		Model:         firstNonEmpty(req.Model, p.cfg.Model),
		MaxTokens:     maxTokens,
		System:        toAnthropicSystem(req.System, req.CacheBoundary),
		Messages:      toAnthropicMessages(req.Messages, thinking != "disabled"),
		Tools:         toAnthropicTools(req.Tools),
		Temperature:   req.Temperature,
		StopSequences: req.Stop,
		Thinking:      thinkingCfg,
		Stream:        stream,
	}
	if thinking == "enabled" || thinking == "adaptive" {
		// The API rejects sampling changes alongside thinking.
		wire.Temperature = nil
	}
	markRecentUserMessages(wire.Messages, req.CacheRecent, MaxCacheBreakpoints-countSystemBreakpoints(wire.System))
	if effort != "" {
		wire.OutputConfig = &anthropicOutput{Effort: effort}
	}
	return wire
}

func toAnthropicSystem(segments []string, cacheBoundary int) []anthropicSystem {
	system := make([]anthropicSystem, len(segments))
	for i, text := range segments {
		system[i] = anthropicSystem{Type: "text", Text: text}
		if i == cacheBoundary-1 {
			system[i].CacheControl = &anthropicCacheCt{Type: "ephemeral"}
		}
	}
	return system
}

func countSystemBreakpoints(system []anthropicSystem) int {
	n := 0
	for _, s := range system {
		if s.CacheControl != nil {
			n++
		}
	}
	return n
}

// markRecentUserMessages sets a cache breakpoint on the last block of the last
// recent user messages, but never more than budget. It works on the final wire
// messages because merging and dropping empty ones changes which messages
// exist.
func markRecentUserMessages(msgs []anthropicMessage, recent, budget int) {
	marks := min(recent, budget)
	for i := len(msgs) - 1; i >= 0 && marks > 0; i-- {
		if msgs[i].Role != RoleUser {
			continue
		}
		last := len(msgs[i].Content) - 1
		msgs[i].Content[last].CacheControl = &anthropicCacheCt{Type: "ephemeral"}
		marks--
	}
}

func toAnthropicTools(tools []ToolSchema) []anthropicTool {
	wire := make([]anthropicTool, len(tools))
	for i, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		wire[i] = anthropicTool{Name: t.Name, Description: t.Description, InputSchema: schema}
	}
	return wire
}

// toAnthropicThinking returns the thinking setting for mode and the max_tokens
// to send with it. The API requires a budget of at least minThinkingBudget
// and below max_tokens, so when the minimum does not fit, max_tokens grows by
// the budget and the reply keeps the room the caller asked for.
func toAnthropicThinking(mode string, maxTokens int) (*anthropicThinking, int) {
	switch mode {
	case "enabled":
		budget := max(minThinkingBudget, maxTokens/2)
		if budget >= maxTokens {
			maxTokens += budget
		}
		return &anthropicThinking{Type: "enabled", BudgetTokens: budget}, maxTokens
	case "adaptive", "disabled":
		return &anthropicThinking{Type: mode}, maxTokens
	}
	return nil, maxTokens
}

// toAnthropicMessages converts history. Messages left with no content after
// filtering are dropped because the API rejects empty ones, and neighbours of
// the same role are merged so roles keep alternating after a drop.
func toAnthropicMessages(msgs []Message, keepThinking bool) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(msgs))
	for _, m := range msgs {
		blocks := toAnthropicBlocks(m.Content, keepThinking)
		switch {
		case len(blocks) == 0:
		case len(out) > 0 && out[len(out)-1].Role == m.Role:
			out[len(out)-1].Content = append(out[len(out)-1].Content, blocks...)
		default:
			out = append(out, anthropicMessage{Role: m.Role, Content: blocks})
		}
	}
	return out
}

func toAnthropicBlocks(blocks []Block, keepThinking bool) []anthropicBlock {
	out := make([]anthropicBlock, 0, len(blocks))
	for _, b := range blocks {
		// Unsigned or another provider's thinking cannot be replayed, and
		// disabled thinking must not see earlier thinking blocks at all.
		if b.Type == BlockThinking && (!keepThinking || b.Signature == "" || foreignThinking(b, FormatAnthropic)) {
			continue
		}
		if b.Type == BlockText && b.Text == "" {
			continue // the API rejects empty text blocks
		}
		out = append(out, toAnthropicBlock(b, keepThinking))
	}
	return out
}

func toAnthropicBlock(b Block, keepThinking bool) anthropicBlock {
	if isRedactedThinking(b) {
		// The API requires redacted thinking back exactly as it sent it.
		return anthropicBlock{Type: anthropicRedactedThinking, Data: b.Signature}
	}
	wire := anthropicBlock{
		Type: b.Type, Text: b.Text, Signature: b.Signature,
		ID: b.ID, Name: b.Name, Input: b.Input,
		ToolUseID: b.ToolUseID, IsError: b.IsError, Source: b.Source,
	}
	if b.Type == BlockThinking {
		// The API requires the field even when the text was omitted.
		wire.Thinking = &b.Thinking
	}
	if b.Type == BlockToolUse && len(b.Input) == 0 {
		wire.Input = json.RawMessage(emptyToolInput)
	}
	if len(b.Content) > 0 {
		wire.Content = toAnthropicBlocks(b.Content, keepThinking)
	}
	return wire
}

// redactedThinkingBlock represents a redacted_thinking reply block: a thinking
// block with no readable text whose encrypted data is replayed verbatim.
func redactedThinkingBlock(data string) Block {
	return Block{
		Type:         BlockThinking,
		Signature:    data,
		ProviderData: map[string]string{anthropicRedactedKey: "true", formatKey: string(FormatAnthropic)},
	}
}

func isRedactedThinking(b Block) bool {
	return b.Type == BlockThinking && b.ProviderData[anthropicRedactedKey] != ""
}
