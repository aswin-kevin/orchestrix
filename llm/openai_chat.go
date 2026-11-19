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
)

// openAIProvider speaks the OpenAI Chat Completions API, which many other
// servers also implement.
type openAIProvider struct {
	cfg *Config
}

// newOpenAI returns an adapter for cfg. It keeps its own copy of the
// configuration, so later changes to the caller's value have no effect.
func newOpenAI(cfg Config) (*openAIProvider, error) {
	if err := cfg.prepare(); err != nil {
		return nil, err
	}
	return &openAIProvider{cfg: &cfg}, nil
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
		body, err := json.Marshal(p.buildRequest(req))
		if err != nil {
			yield(StreamEvent{}, fmt.Errorf("llm: encode openai request: %w", err))
			return
		}
		for attempt := 0; ; attempt++ {
			gate := &emptyReplyGate{yield: yield}
			err := p.streamAttempt(ctx, body, gate)
			if gate.consumerStopped {
				return
			}
			if err != nil {
				gate.flush()
				yield(StreamEvent{}, err)
				return
			}
			if gate.opened || attempt >= p.emptyRetries() {
				gate.flush()
				return
			}
			if err := sleepContext(ctx, p.cfg.retryDelay(attempt, nil)); err != nil {
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
	resp, err := p.send(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return streamOpenAIChunks(ctx, resp.Body, gate.emit)
}

// emptyReplyGate holds back events until the reply proves non-empty. Until
// then the consumer has seen nothing, so the request can still be repeated.
// Thinking alone does not count as content.
type emptyReplyGate struct {
	yield           func(StreamEvent, error) bool
	pending         []StreamEvent
	opened          bool
	consumerStopped bool
}

// emit forwards ev, or buffers it while the reply is still empty. It reports
// whether the consumer wants more events.
func (g *emptyReplyGate) emit(ev StreamEvent) bool {
	if !g.opened && (ev.Type == EvTextDelta || ev.Type == EvToolStart) {
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

// streamOpenAIChunks translates the SSE body into events passed to emit. A
// body that ends with neither [DONE] nor a finish_reason is reported as an
// error so truncated replies are never mistaken for complete ones. It returns
// nil without further reading once emit reports the consumer is gone.
func streamOpenAIChunks(ctx context.Context, body io.Reader, emit func(StreamEvent) bool) error {
	state := newOpenAIStreamState()
	if !emit(StreamEvent{Type: EvMessageStart}) {
		return nil
	}
	for sse, err := range readSSE(body) {
		if err != nil {
			return streamReadError(ctx, err)
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
	startedTools map[int]bool
	finishReason string
	usage        Usage
}

func newOpenAIStreamState() *openAIStreamState {
	return &openAIStreamState{startedTools: make(map[int]bool)}
}

// translate maps one chunk to stream events.
func (s *openAIStreamState) translate(data string) ([]StreamEvent, error) {
	var chunk openAIChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, fmt.Errorf("llm: decode openai chunk: %w", err)
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		// Status 0: the HTTP exchange itself succeeded.
		return nil, &APIError{Body: data}
	}
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
		events = append(events, StreamEvent{Type: EvTextDelta, Index: openAITextIndex, Text: d.Content})
	}
	if thinking := firstNonEmpty(d.ReasoningContent, d.Reasoning); thinking != "" {
		events = append(events, StreamEvent{Type: EvThinkingDelta, Index: openAIThinkingIndex, Text: thinking})
	}
	for _, call := range d.ToolCalls {
		events = append(events, s.toolCallEvents(call)...)
	}
	return events
}

func (s *openAIStreamState) toolCallEvents(call openAIToolCallDelta) []StreamEvent {
	index := openAIToolIndexBase + call.Index
	var events []StreamEvent
	if !s.startedTools[call.Index] {
		s.startedTools[call.Index] = true
		// Some servers omit call ids, yet a result can only be matched to
		// its call by id.
		id := firstNonEmpty(call.ID, fmt.Sprintf("call_%d", call.Index))
		events = append(events, StreamEvent{Type: EvToolStart, Index: index, ToolID: id, ToolName: call.Function.Name})
	}
	if extra := call.ExtraContent; len(extra) > 0 && string(extra) != "null" {
		events = append(events, StreamEvent{
			Type: EvProviderData, Index: index, Key: openAIExtraContentKey, Value: string(extra),
		})
	}
	if call.Function.Arguments != "" {
		events = append(events, StreamEvent{Type: EvToolInputDelta, Index: index, Text: call.Function.Arguments})
	}
	return events
}

// complete emits the closing events.
func (s *openAIStreamState) complete(emit func(StreamEvent) bool) {
	final := StreamEvent{
		Type:       EvMessageDelta,
		StopReason: s.stopReason(),
		Usage:      s.usage,
	}
	if emit(final) {
		emit(StreamEvent{Type: EvMessageStop})
	}
}

// stopReason maps the wire finish_reason to a StopReason constant. Some
// servers report "stop" even when the model called tools, which would hide
// the calls from a loop keyed on the stop reason.
func (s *openAIStreamState) stopReason() string {
	switch s.finishReason {
	case "stop":
		if len(s.startedTools) > 0 {
			return StopToolUse
		}
		return StopEndTurn
	case "tool_calls", "function_call":
		return StopToolUse
	case "length":
		return StopMaxTokens
	}
	return s.finishReason
}

// send issues the HTTP request, returning the 2xx response.
func (p *openAIProvider) send(ctx context.Context, body []byte) (*http.Response, error) {
	return p.cfg.do(ctx, func() (*http.Request, error) {
		httpReq, err := http.NewRequest(http.MethodPost, p.completionsURL(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		p.setHeaders(httpReq)
		return httpReq, nil
	})
}

func (p *openAIProvider) completionsURL() string {
	return strings.TrimRight(p.cfg.BaseURL, "/") + "/chat/completions"
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
		ReasoningEffort: firstNonEmpty(req.ReasoningEffort, p.cfg.ReasoningEffort),
		Stream:          true,
		StreamOptions:   openAIStreamOptions{IncludeUsage: true},
	}
	if firstNonEmpty(p.cfg.MaxTokensField, DefaultMaxTokensField) == openAIMaxCompletionsField {
		wire.MaxCompletionTokens = maxTokens
	} else {
		wire.MaxTokens = maxTokens
	}
	if thinking := firstNonEmpty(req.Thinking, p.cfg.Thinking); thinking != "" {
		wire.Thinking = &openAIThinking{Type: thinking}
	}
	return wire
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
