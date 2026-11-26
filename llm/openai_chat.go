package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strings"
)

const (
	// openAIExtraContentKey is the ProviderData key under which a tool call's
	// extra_content is stored and replayed.
	openAIExtraContentKey = "openai.extra_content"

	// openAIMaxCompletionsField is the MaxTokensField value that selects
	// max_completion_tokens in place of max_tokens.
	openAIMaxCompletionsField = "max_completion_tokens"

	// openAIDoneSentinel is the data of the event that ends a stream.
	openAIDoneSentinel = "[DONE]"

	// openAIImageOmitted stands in for non-text tool result content, which
	// the tool role cannot carry.
	openAIImageOmitted = "[image omitted]"

	// Stream block indexes. Text and thinking each have a fixed slot, and tool
	// calls start at openAIToolIndexBase so their provider index can never
	// collide with either.
	openAITextIndex     = 0
	openAIThinkingIndex = 1
	openAIToolIndexBase = 1000

	// openAIHost is the host of the OpenAI API itself, whose reasoning
	// models reject max_tokens.
	openAIHost = "api.openai.com"
)

// openAIProvider speaks the OpenAI Chat Completions API, which many other
// servers also implement.
type openAIProvider struct {
	cfg Config
	t   *Transport
}

// newOpenAI returns an adapter for cfg. It keeps its own copy of the
// configuration, so later changes to the caller's value have no effect.
func newOpenAI(cfg Config) (*openAIProvider, error) {
	t, err := NewTransport(cfg)
	if err != nil {
		return nil, err
	}
	return &openAIProvider{cfg: t.Config(), t: t}, nil
}

// configuredModel implements modelReporter.
func (p *openAIProvider) configuredModel() string { return p.cfg.Model }

// Wire types. They exist so that local-only fields (ProviderData, Meta) can
// never reach the network.

type openAIRequest struct {
	Model               string              `json:"model"`
	Messages            []openAIMessage     `json:"messages"`
	Tools               []openAITool        `json:"tools,omitempty"`
	MaxTokens           int                 `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                 `json:"max_completion_tokens,omitempty"`
	Temperature         *float64            `json:"temperature,omitempty"`
	Stop                []string            `json:"stop,omitempty"`
	ReasoningEffort     string              `json:"reasoning_effort,omitempty"`
	Thinking            *openAIThinking     `json:"thinking,omitempty"`
	ToolChoice          any                 `json:"tool_choice,omitempty"`
	Stream              bool                `json:"stream"`
	StreamOptions       openAIStreamOptions `json:"stream_options"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIThinking struct {
	Type string `json:"type"`
}

// openAIMessage is one chat message. Content is a string, a []openAIPart
// (user messages with images) or nil (assistant messages with only tool
// calls).
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

type openAIToolCall struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Function     openAIFunctionCall `json:"function"`
	ExtraContent json.RawMessage    `json:"extra_content,omitempty"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

// openAIChunk is one streamed chunk. A final chunk may carry only usage, with
// no choices.
type openAIChunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Error   json.RawMessage `json:"error"`
	Choices []struct {
		Delta        openAIDelta `json:"delta"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

type openAIDelta struct {
	Content          string                `json:"content"`
	ReasoningContent string                `json:"reasoning_content"`
	Reasoning        string                `json:"reasoning"`
	ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
}

type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	ExtraContent json.RawMessage `json:"extra_content"`
}

// Complete implements Provider by draining a stream, so a non-streaming call
// gets the same empty-response retry as a streaming one.
func (p *openAIProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	return Collect(ctx, p, req)
}

// Stream implements Provider. Retries cover establishing the response and
// replies that turn out empty; a stream that breaks after content has been
// delivered surfaces as an error and is not replayed.
func (p *openAIProvider) Stream(ctx context.Context, req Request) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		body, err := encodeBody(req, p.buildRequest(req))
		if err != nil {
			yield(StreamEvent{}, fmt.Errorf("llm: encode openai request: %w", err))
			return
		}
		// discarded totals the usage of empty attempts, which were billed
		// even though the consumer never sees them.
		var discarded Usage
		for attempt := 0; ; attempt++ {
			gate := &emptyReplyGate{yield: yield, carried: discarded}
			err := p.streamAttempt(ctx, body, gate)
			if gate.consumerStopped {
				return
			}
			if err != nil {
				gate.flush()
				yield(StreamEvent{}, err)
				return
			}
			if gate.opened || !gate.retryable() || attempt >= p.emptyRetries() {
				gate.flush()
				return
			}
			discarded = gate.usage
			if err := sleepContext(ctx, p.t.retryDelay(attempt, nil)); err != nil {
				yield(StreamEvent{}, err)
				return
			}
		}
	}
}

// emptyRetries is the number of re-requests allowed for an empty reply.
func (p *openAIProvider) emptyRetries() int {
	switch {
	case p.cfg.EmptyResponseRetries == 0:
		return DefaultEmptyResponseRetries
	case p.cfg.EmptyResponseRetries < 0:
		return 0
	}
	return p.cfg.EmptyResponseRetries
}

// streamAttempt issues one request and feeds its events to gate. The body is
// closed on every exit, including the consumer breaking out of the loop,
// which releases the connection promptly.
func (p *openAIProvider) streamAttempt(ctx context.Context, body []byte, gate *emptyReplyGate) error {
	return streamOpenAIChunks(p.t, p.t.Stream(ctx, p.request(body)), gate.emit)
}

// emptyReplyGate holds back events until the reply proves non-empty. Until
// then the consumer has seen nothing, so the request can still be repeated.
// Thinking alone does not count as content.
type emptyReplyGate struct {
	yield           func(StreamEvent, error) bool
	pending         []StreamEvent
	opened          bool
	consumerStopped bool

	// carried is the usage of earlier discarded attempts, added to this
	// attempt's message delta.
	carried Usage
	// stopReason and usage are those of this attempt's message delta, with
	// carried included in usage.
	stopReason StopReason
	usage      Usage
}

// retryable reports whether an empty reply may be requested again. Only a
// reply that ended normally qualifies: one cut off by the output limit or a
// filter would most likely end the same way, and be billed again.
func (g *emptyReplyGate) retryable() bool {
	return g.stopReason == StopEndTurn || g.stopReason == ""
}

// emit forwards ev, or buffers it while the reply is still empty. It reports
// whether the consumer wants more events.
func (g *emptyReplyGate) emit(ev StreamEvent) bool {
	if ev.Type == EventMessageDelta {
		ev.Usage.Add(g.carried)
		g.stopReason, g.usage = ev.StopReason, ev.Usage
	}
	if !g.opened && (ev.Type == EventTextDelta || ev.Type == EventToolStart) {
		g.opened = true
		if !g.flush() {
			return false
		}
	}
	if !g.opened {
		g.pending = append(g.pending, ev)
		return true
	}
	if !g.yield(ev, nil) {
		g.consumerStopped = true
		return false
	}
	return true
}

// flush yields the buffered events and reports whether the consumer wants
// more.
func (g *emptyReplyGate) flush() bool {
	for _, ev := range g.pending {
		if !g.yield(ev, nil) {
			g.consumerStopped = true
			return false
		}
	}
	g.pending = nil
	return true
}

// streamOpenAIChunks translates the SSE events into events passed to emit,
// starting with a message start sent when the first event arrives (or when
// the body ends with no events; a read error before any event sends none). A
// body that ends with neither [DONE] nor a finish_reason is reported as an
// error so truncated replies are never mistaken for complete ones. It returns nil
// without further reading once emit reports the consumer is gone.
func streamOpenAIChunks(t *Transport, events iter.Seq2[SSEEvent, error], emit func(StreamEvent) bool) error {
	state := newOpenAIStreamState(t)
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
		if strings.TrimSpace(sse.Data) == openAIDoneSentinel {
			state.complete(emit)
			return nil
		}
		events, err := state.translate(sse.Data)
		for _, ev := range events {
			if !emit(ev) {
				return nil
			}
		}
		if err != nil {
			return err
		}
	}
	if !started && !emit(StreamEvent{Type: EventMessageStart}) {
		return nil
	}
	if state.finishReason == "" {
		return errors.New("llm: openai stream ended before finish_reason or [DONE]")
	}
	state.complete(emit)
	return nil
}

// openAIStreamState carries what must survive between chunks. Usage and the
// stop reason are held until the stream ends because they may arrive in
// separate chunks and some servers repeat usage in every chunk.
type openAIStreamState struct {
	// t redacts credentials from errors reported in the stream.
	t *Transport
	// slots maps a wire tool-call index to the call currently open there.
	slots map[int]*toolSlot
	// next is the number of calls started so far.
	next int
	// runID makes synthesised call ids unique across streams.
	runID        string
	finishReason string
	usage        Usage
	// model and id are the first the chunks reported, reported with the
	// closing events.
	model string
	id    string
}

// toolSlot is one started tool call: the id it was given, whether that id
// was synthesised, and its stream block index.
type toolSlot struct {
	id        string
	synthetic bool
	index     int
}

func newOpenAIStreamState(t *Transport) *openAIStreamState {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return &openAIStreamState{t: t, slots: make(map[int]*toolSlot), runID: hex.EncodeToString(b[:])}
}

// translate maps one chunk to stream events.
func (s *openAIStreamState) translate(data string) ([]StreamEvent, error) {
	var chunk openAIChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, fmt.Errorf("llm: decode openai chunk: %w", err)
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		// Status 0: the HTTP exchange itself succeeded.
		return nil, s.t.StreamError(data)
	}
	s.model = firstNonEmpty(s.model, chunk.Model)
	s.id = firstNonEmpty(s.id, chunk.ID)
	if u := chunk.Usage; u != nil {
		cached := u.PromptTokensDetails.CachedTokens
		s.usage = Usage{
			Input:     max(u.PromptTokens-cached, 0),
			Output:    u.CompletionTokens,
			CacheRead: cached,
		}
	}
	if len(chunk.Choices) == 0 {
		return nil, nil
	}
	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		s.finishReason = choice.FinishReason
	}
	return s.deltaEvents(choice.Delta), nil
}

func (s *openAIStreamState) deltaEvents(d openAIDelta) []StreamEvent {
	var events []StreamEvent
	if d.Content != "" {
		events = append(events, StreamEvent{Type: EventTextDelta, Index: openAITextIndex, Text: d.Content})
	}
	if thinking := firstNonEmpty(d.ReasoningContent, d.Reasoning); thinking != "" {
		events = append(events, StreamEvent{Type: EventThinkingDelta, Index: openAIThinkingIndex, Text: thinking})
	}
	for _, call := range d.ToolCalls {
		events = append(events, s.toolCallEvents(call)...)
	}
	return events
}

// toolCallEvents maps one tool-call delta. Some servers omit the wire index,
// so every parallel call arrives at index 0: a new, different id at an index
// therefore starts a new call, while a delta without an id continues the
// call open there.
func (s *openAIStreamState) toolCallEvents(call openAIToolCallDelta) []StreamEvent {
	var events []StreamEvent
	slot := s.slots[call.Index]
	if slot == nil || s.startsNewCall(slot, call) {
		// Some servers omit call ids, yet a result can only be matched to
		// its call by id, and ids must not repeat across turns.
		id, synthetic := call.ID, false
		if id == "" {
			id, synthetic = fmt.Sprintf("call_%s_%d", s.runID, s.next), true
		}
		slot = &toolSlot{id: id, synthetic: synthetic, index: openAIToolIndexBase + s.next}
		s.slots[call.Index] = slot
		s.next++
		events = append(events, StreamEvent{Type: EventToolStart, Index: slot.index, ToolID: id, ToolName: call.Function.Name})
	}
	index := slot.index
	if extra := call.ExtraContent; len(extra) > 0 && string(extra) != "null" {
		events = append(events, StreamEvent{
			Type: EventProviderData, Index: index, Key: openAIExtraContentKey, Value: string(extra),
		})
	}
	if call.Function.Arguments != "" {
		events = append(events, StreamEvent{Type: EventToolInputDelta, Index: index, Text: call.Function.Arguments})
	}
	return events
}

// startsNewCall reports whether call, at the index where slot is open,
// begins another call. It must bring a different id; when slot's id was
// synthesised, the id may simply arrive late for the same call, so it must
// also name a function.
func (s *openAIStreamState) startsNewCall(slot *toolSlot, call openAIToolCallDelta) bool {
	if call.ID == "" || call.ID == slot.id {
		return false
	}
	return !slot.synthetic || call.Function.Name != ""
}

// complete emits the closing events.
func (s *openAIStreamState) complete(emit func(StreamEvent) bool) {
	final := StreamEvent{
		Type:       EventMessageDelta,
		StopReason: s.stopReason(),
		Usage:      s.usage,
		Model:      s.model,
		ResponseID: s.id,
	}
	if emit(final) {
		emit(StreamEvent{Type: EventMessageStop})
	}
}

// stopReason maps the wire finish_reason to a StopReason constant. Some
// servers report "stop" even when the model called tools, which would hide
// the calls from a loop keyed on the stop reason, and some end with [DONE]
// and no finish_reason at all.
func (s *openAIStreamState) stopReason() StopReason {
	switch s.finishReason {
	case "stop", "":
		if s.next > 0 {
			return StopToolUse
		}
		return StopEndTurn
	case "tool_calls", "function_call":
		return StopToolUse
	case "length":
		return StopMaxTokens
	case "content_filter":
		return StopContentFilter
	}
	return StopReason(s.finishReason)
}

// request returns the func that builds the HTTP request for body, once per
// attempt.
func (p *openAIProvider) request(body []byte) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		target, err := endpoint(p.cfg.BaseURL, "chat/completions")
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

func (p *openAIProvider) setHeaders(r *http.Request) {
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

func (p *openAIProvider) buildRequest(req Request) openAIRequest {
	maxTokens := firstNonZero(req.MaxTokens, p.cfg.DefaultMaxTokens, DefaultMaxTokens)
	wire := openAIRequest{
		Model:           firstNonEmpty(req.Model, p.cfg.Model),
		Messages:        toOpenAIMessages(req.System, req.Messages),
		Tools:           toOpenAITools(req.Tools),
		Temperature:     req.Temperature,
		Stop:            req.Stop,
		ToolChoice:      openAIToolChoice(req.ToolChoice),
		ReasoningEffort: string(firstNonEmpty(req.ReasoningEffort, p.cfg.ReasoningEffort)),
		Stream:          true,
		StreamOptions:   openAIStreamOptions{IncludeUsage: true},
	}
	if p.maxTokensField() == openAIMaxCompletionsField {
		wire.MaxCompletionTokens = maxTokens
	} else {
		wire.MaxTokens = maxTokens
	}
	if thinking := firstNonEmpty(req.Thinking, p.cfg.Thinking); thinking != "" {
		wire.Thinking = &openAIThinking{Type: string(thinking)}
	}
	return wire
}

// maxTokensField is the configured output-limit field. Unset, it is
// max_completion_tokens for the OpenAI API itself, whose reasoning models
// reject max_tokens, and DefaultMaxTokensField elsewhere.
func (p *openAIProvider) maxTokensField() string {
	if p.cfg.MaxTokensField != "" {
		return p.cfg.MaxTokensField
	}
	if u, err := url.Parse(p.cfg.BaseURL); err == nil && strings.EqualFold(u.Hostname(), openAIHost) {
		return openAIMaxCompletionsField
	}
	return DefaultMaxTokensField
}

func toOpenAITools(tools []ToolSchema) []openAITool {
	wire := make([]openAITool, len(tools))
	for i, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		wire[i] = openAITool{
			Type:     "function",
			Function: openAIFunction{Name: t.Name, Description: t.Description, Parameters: schema},
		}
	}
	return wire
}

// toOpenAIMessages converts history. Empty messages are dropped because
// servers reject them, and consecutive assistant messages are merged so that
// a dropped message between them leaves a valid sequence.
func toOpenAIMessages(system []string, msgs []Message) []openAIMessage {
	var out []openAIMessage
	if text := joinNonEmpty(system, "\n\n"); text != "" {
		out = append(out, openAIMessage{Role: "system", Content: text})
	}
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			out = appendAssistant(out, m)
			continue
		}
		out = append(out, toOpenAIUserMessages(m)...)
	}
	return out
}

// toOpenAIUserMessages turns a user message into tool messages followed by
// the user's own content: a tool message must directly follow the assistant
// message that made the call, so results go first.
func toOpenAIUserMessages(m Message) []openAIMessage {
	var out []openAIMessage
	var parts []openAIPart
	for _, b := range m.Content {
		switch b.Type {
		case BlockToolResult:
			out = append(out, openAIMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: toolResultText(b)})
		case BlockText:
			if b.Text != "" {
				parts = append(parts, openAIPart{Type: "text", Text: b.Text})
			}
		case BlockImage:
			if url := imageURL(b.Source); url != "" {
				parts = append(parts, openAIPart{Type: "image_url", ImageURL: &openAIImageURL{URL: url}})
			}
		}
	}
	switch {
	case len(parts) == 0:
	case len(parts) == 1 && parts[0].Type == "text":
		// A plain string is understood by every compatible server; the
		// part array is not.
		out = append(out, openAIMessage{Role: "user", Content: parts[0].Text})
	default:
		out = append(out, openAIMessage{Role: "user", Content: parts})
	}
	return out
}

// imageURL returns the URL for an image source, encoding base64 data inline.
func imageURL(src *Source) string {
	switch {
	case src == nil:
		return ""
	case src.Type == "base64":
		return "data:" + src.MediaType + ";base64," + src.Data
	}
	return src.URL
}

// toolResultText flattens a tool result to the plain text the tool role
// accepts.
func toolResultText(b Block) string {
	texts := make([]string, len(b.Content))
	for i, c := range b.Content {
		if c.Type == BlockText {
			texts[i] = c.Text
		} else {
			texts[i] = openAIImageOmitted
		}
	}
	return strings.Join(texts, "\n")
}

// appendAssistant converts m and appends it, merging into a directly
// preceding assistant message.
func appendAssistant(out []openAIMessage, m Message) []openAIMessage {
	msg, ok := toOpenAIAssistant(m)
	if !ok {
		return out
	}
	if last := len(out) - 1; last >= 0 && out[last].Role == "assistant" {
		out[last] = mergeAssistant(out[last], msg)
		return out
	}
	return append(out, msg)
}

// toOpenAIAssistant converts an assistant message. Thinking blocks are never
// sent. ok is false when nothing is left to send.
func toOpenAIAssistant(m Message) (msg openAIMessage, ok bool) {
	msg = openAIMessage{Role: "assistant"}
	var text strings.Builder
	for _, b := range m.Content {
		switch b.Type {
		case BlockText:
			text.WriteString(b.Text)
		case BlockToolUse:
			msg.ToolCalls = append(msg.ToolCalls, toOpenAIToolCall(b))
		}
	}
	if text.Len() > 0 {
		msg.Content = text.String()
	}
	return msg, msg.Content != nil || len(msg.ToolCalls) > 0
}

func toOpenAIToolCall(b Block) openAIToolCall {
	args := string(b.Input)
	if args == "" {
		args = emptyToolInput
	}
	call := openAIToolCall{
		ID:       b.ID,
		Type:     "function",
		Function: openAIFunctionCall{Name: b.Name, Arguments: args},
	}
	if extra := b.ProviderData[openAIExtraContentKey]; extra != "" {
		call.ExtraContent = json.RawMessage(extra)
	}
	return call
}

func mergeAssistant(a, b openAIMessage) openAIMessage {
	a.ToolCalls = append(a.ToolCalls, b.ToolCalls...)
	switch {
	case a.Content == nil:
		a.Content = b.Content
	case b.Content != nil:
		a.Content = a.Content.(string) + "\n\n" + b.Content.(string)
	}
	return a
}

// joinNonEmpty joins the non-empty strings with sep.
func joinNonEmpty(parts []string, sep string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
