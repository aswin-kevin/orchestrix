package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sseServer replies to every request with the given stream body and records
// the last request body and headers.
type sseServer struct {
	*httptest.Server
	hits    atomic.Int32
	body    map[string]any
	headers http.Header
	path    string
}

func newSSEServer(t *testing.T, status int, contentType, reply string) *sseServer {
	t.Helper()
	s := &sseServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		s.body = map[string]any{}
		_ = json.Unmarshal(raw, &s.body)
		s.headers = r.Header.Clone()
		s.path = r.URL.Path
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(s.Close)
	return s
}

func newTestAnthropic(t *testing.T, baseURL string) *anthropicProvider {
	t.Helper()
	p, err := newAnthropic(Config{
		Format:     FormatAnthropic,
		BaseURL:    baseURL,
		APIKey:     "key-1",
		Model:      "claude-test",
		MaxRetries: -1,
		Headers:    map[string]string{"X-Extra": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const textOnlyStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicRequestShape(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", textOnlyStream)
	p := newTestAnthropic(t, srv.URL+"/v1")

	req := Request{
		System:        []string{"first", "second"},
		CacheBoundary: 1,
		Messages: []Message{
			UserText("hello"),
			{Role: RoleAssistant, Content: []Block{{
				Type: BlockText, Text: "hi", ProviderData: map[string]string{"secret": "token"},
			}}},
		},
		Tools: []ToolSchema{{Name: "ls", Description: "list", InputSchema: map[string]any{"type": "object"}}},
		Stop:  []string{"END"},
	}
	if _, err := Collect(context.Background(), p, req); err != nil {
		t.Fatal(err)
	}

	if srv.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", srv.path)
	}
	for k, want := range map[string]string{
		"X-Api-Key": "key-1", "Anthropic-Version": defaultAnthropicVersion,
		"Content-Type": "application/json", "X-Extra": "yes",
	} {
		if got := srv.headers.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if srv.body["model"] != "claude-test" || srv.body["stream"] != true {
		t.Errorf("model/stream = %v/%v", srv.body["model"], srv.body["stream"])
	}
	if srv.body["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v", srv.body["max_tokens"])
	}
	system := srv.body["system"].([]any)
	if len(system) != 2 {
		t.Fatalf("system = %v", system)
	}
	if system[0].(map[string]any)["cache_control"] == nil {
		t.Error("first system segment lacks cache_control")
	}
	if system[1].(map[string]any)["cache_control"] != nil {
		t.Error("second system segment has cache_control")
	}
	tool := srv.body["tools"].([]any)[0].(map[string]any)
	if tool["input_schema"] == nil || tool["name"] != "ls" {
		t.Errorf("tool = %v", tool)
	}
	raw, _ := json.Marshal(srv.body)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "provider_data") {
		t.Errorf("ProviderData leaked into body: %s", raw)
	}
	if srv.body["stop_sequences"].([]any)[0] != "END" {
		t.Errorf("stop_sequences = %v", srv.body["stop_sequences"])
	}
}

func TestAnthropicThinkingAndHistory(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", textOnlyStream)
	p := newTestAnthropic(t, srv.URL)

	history := []Message{
		UserText("q"),
		{Role: RoleAssistant, Content: []Block{
			{Type: BlockThinking, Thinking: "signed", Signature: "sig"},
			{Type: BlockThinking, Thinking: "unsigned"},
			TextBlock("a"),
		}},
		UserText("again"),
	}
	run := func(thinking string) []any {
		req := Request{Messages: history, MaxTokens: 4000, Thinking: thinking, ReasoningEffort: "high"}
		if _, err := Collect(context.Background(), p, req); err != nil {
			t.Fatal(err)
		}
		return srv.body["messages"].([]any)[1].(map[string]any)["content"].([]any)
	}

	if got := run("enabled"); len(got) != 2 || got[0].(map[string]any)["signature"] != "sig" {
		t.Errorf("enabled history = %v", got)
	}
	thinking := srv.body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(2000) {
		t.Errorf("thinking = %v", thinking)
	}
	if srv.body["output_config"].(map[string]any)["effort"] != "high" {
		t.Errorf("output_config = %v", srv.body["output_config"])
	}
	if got := run("disabled"); len(got) != 1 {
		t.Errorf("disabled history = %v", got)
	}
	if srv.body["thinking"].(map[string]any)["type"] != "disabled" {
		t.Errorf("thinking = %v", srv.body["thinking"])
	}
	run("")
	if _, ok := srv.body["thinking"]; ok {
		t.Error("thinking present for empty setting")
	}
}

const richStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":4}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"let me "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"look"}}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu_1","name":"ls","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"th\":\"/\"}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicStreamTextToolThinking(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", richStream)
	p := newTestAnthropic(t, srv.URL)

	resp, err := Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}})
	if err != nil {
		t.Fatal(err)
	}
	blocks := resp.Message.Content
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[0].Type != BlockThinking || blocks[0].Thinking != "hmm" || blocks[0].Signature != "sig" {
		t.Errorf("thinking block = %+v", blocks[0])
	}
	if blocks[1].Text != "let me look" {
		t.Errorf("text block = %+v", blocks[1])
	}
	if blocks[2].ID != "tu_1" || blocks[2].Name != "ls" || string(blocks[2].Input) != `{"path":"/"}` {
		t.Errorf("tool block = %+v", blocks[2])
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if want := (Usage{Input: 12, Output: 30, CacheRead: 4}); resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestAnthropicErrorEventOverflow(t *testing.T) {
	stream := `event: error
data: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens"}}

`
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	p := newTestAnthropic(t, srv.URL)

	_, err := Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}})
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsContextOverflow(err) {
		t.Errorf("IsContextOverflow(%v) = false", err)
	}
}

func TestAnthropicComplete(t *testing.T) {
	reply := `{"content":[
		{"type":"thinking","thinking":"hmm","signature":"sig"},
		{"type":"text","text":"done"},
		{"type":"tool_use","id":"tu_1","name":"ls","input":{"path":"/"}}],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":7,"output_tokens":9,"cache_creation_input_tokens":3}}`
	srv := newSSEServer(t, 200, "application/json", reply)
	p := newTestAnthropic(t, srv.URL)

	resp, err := p.Complete(context.Background(), Request{Messages: []Message{UserText("x")}})
	if err != nil {
		t.Fatal(err)
	}
	if srv.body["stream"] == true {
		t.Error("Complete sent stream:true")
	}
	blocks := resp.Message.Content
	if len(blocks) != 3 || blocks[0].Signature != "sig" || blocks[1].Text != "done" ||
		string(blocks[2].Input) != `{"path":"/"}` {
		t.Errorf("blocks = %+v", blocks)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if want := (Usage{Input: 7, Output: 9, CacheWrite: 3}); resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestAnthropicMidStreamDropNotRetried(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}

`
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	p, err := newAnthropic(Config{Format: FormatAnthropic, BaseURL: srv.URL, Model: "m", MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}}); err == nil {
		t.Fatal("expected error for truncated stream")
	}
	if got := srv.hits.Load(); got != 1 {
		t.Errorf("server hit %d times, want 1", got)
	}
}

func TestNewValidation(t *testing.T) {
	cases := map[string]Config{
		"no model":       {Format: FormatAnthropic},
		"unknown format": {Format: "bogus", Model: "m"},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	for _, format := range []Format{FormatAnthropic, FormatOpenAI, FormatOpenAIResponses} {
		if _, err := New(Config{Format: format, Model: "m"}); err != nil {
			t.Errorf("%s: %v", format, err)
		}
	}
}

// newBlockingServer sends first and then holds the connection open, reporting
// when the client goes away.
func newBlockingServer(t *testing.T, first string) (url string, gone <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(ch)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, ch
}

func waitClosed(t *testing.T, gone <-chan struct{}) {
	t.Helper()
	select {
	case <-gone:
	case <-time.After(3 * time.Second):
		t.Fatal("server connection was not closed")
	}
}

const anthropicFirstDelta = `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}

`

func TestAnthropicStreamConsumerBreakClosesConnection(t *testing.T) {
	url, gone := newBlockingServer(t, anthropicFirstDelta)
	p := newTestAnthropic(t, url)

	for _, err := range p.Stream(context.Background(), Request{Messages: []Message{UserText("x")}}) {
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	waitClosed(t, gone)
}

func TestAnthropicStreamContextCancelClosesConnection(t *testing.T) {
	url, gone := newBlockingServer(t, anthropicFirstDelta)
	p := newTestAnthropic(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var last error
	for _, err := range p.Stream(ctx, Request{Messages: []Message{UserText("x")}}) {
		if err == nil {
			cancel()
		}
		last = err
	}
	if !errors.Is(last, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", last)
	}
	waitClosed(t, gone)
}

func TestAnthropicHistoryMergesAndDropsEmpty(t *testing.T) {
	msgs := toAnthropicMessages([]Message{
		UserText("one"),
		{Role: RoleAssistant, Content: []Block{{Type: BlockThinking, Thinking: "unsigned"}}},
		UserText("two"),
		{Role: RoleAssistant, Content: []Block{TextBlock("")}},
	}, true)
	if len(msgs) != 1 || len(msgs[0].Content) != 2 || msgs[0].Role != RoleUser {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestAnthropicToolDefaultSchema(t *testing.T) {
	tools := toAnthropicTools([]ToolSchema{{Name: "noargs"}})
	if tools[0].InputSchema["type"] != "object" {
		t.Errorf("schema = %v", tools[0].InputSchema)
	}
}
