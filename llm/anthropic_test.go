package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

const redactedStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":3}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"ENCRYPTED"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"ls","input":{}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

// TestAnthropicRedactedThinkingRoundTrip checks that redacted thinking, which
// the API requires back unchanged alongside tool use, survives a streamed
// reply and is replayed with its encrypted data intact.
func TestAnthropicRedactedThinkingRoundTrip(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", redactedStream)
	p := newTestAnthropic(t, srv.URL)

	resp, err := Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}})
	if err != nil {
		t.Fatal(err)
	}
	blocks := resp.Message.Content
	if len(blocks) != 2 || !isRedactedThinking(blocks[0]) || blocks[0].Signature != "ENCRYPTED" {
		t.Fatalf("blocks = %+v", blocks)
	}

	history := []Message{
		UserText("x"), resp.Message,
		{Role: RoleUser, Content: []Block{ToolResultText("tu_1", "ok", false)}},
	}
	if _, err := Collect(context.Background(), p, Request{Messages: history, Thinking: "enabled"}); err != nil {
		t.Fatal(err)
	}
	replayed := srv.body["messages"].([]any)[1].(map[string]any)["content"].([]any)[0]
	want := map[string]any{"type": "redacted_thinking", "data": "ENCRYPTED"}
	if !reflect.DeepEqual(replayed, want) {
		t.Errorf("replayed block = %v, want %v", replayed, want)
	}
}

func TestAnthropicCompleteRedactedThinking(t *testing.T) {
	reply := `{"content":[
		{"type":"redacted_thinking","data":"ENCRYPTED"},
		{"type":"text","text":"done"}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	srv := newSSEServer(t, 200, "application/json", reply)
	p := newTestAnthropic(t, srv.URL)

	resp, err := p.Complete(context.Background(), Request{Messages: []Message{UserText("x")}})
	if err != nil {
		t.Fatal(err)
	}
	blocks := resp.Message.Content
	if len(blocks) != 2 || !isRedactedThinking(blocks[0]) || blocks[0].Signature != "ENCRYPTED" {
		t.Errorf("blocks = %+v", blocks)
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

// cacheMarkers returns, per wire message, whether its last block carries a
// cache breakpoint, plus the total number of breakpoints including system.
func cacheMarkers(t *testing.T, body map[string]any) (perMessage []bool, total int) {
	t.Helper()
	system, _ := body["system"].([]any) // omitted when empty
	for _, s := range system {
		if _, ok := s.(map[string]any)["cache_control"]; ok {
			total++
		}
	}
	for _, m := range body["messages"].([]any) {
		blocks := m.(map[string]any)["content"].([]any)
		marked := false
		for i, b := range blocks {
			if _, ok := b.(map[string]any)["cache_control"]; ok {
				if i != len(blocks)-1 {
					t.Errorf("cache_control on block %d, want only the last of %d", i, len(blocks))
				}
				marked = true
				total++
			}
		}
		perMessage = append(perMessage, marked)
	}
	return perMessage, total
}

func sendCacheRequest(t *testing.T, req Request) map[string]any {
	t.Helper()
	srv := newSSEServer(t, 200, "text/event-stream", textOnlyStream)
	p := newTestAnthropic(t, srv.URL+"/v1")
	if _, err := Collect(context.Background(), p, req); err != nil {
		t.Fatal(err)
	}
	return srv.body
}

func toolResultMessage(id string) Message {
	return Message{Role: RoleUser, Content: []Block{{
		Type: BlockToolResult, ToolUseID: id, Content: []Block{{Type: BlockText, Text: "ok"}},
	}}}
}

func TestAnthropicCacheRecentMarksLastUserMessages(t *testing.T) {
	asst := Message{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "a"}}}
	msgs := []Message{UserText("u1"), asst, toolResultMessage("t1"), asst, UserText("u3")}

	got, total := cacheMarkers(t, sendCacheRequest(t, Request{Messages: msgs, CacheRecent: 2}))
	want := []bool{false, false, true, false, true}
	if !equalBools(got, want) || total != 2 {
		t.Errorf("markers = %v (total %d), want %v (total 2)", got, total, want)
	}
}

func TestAnthropicCacheRecentIsCappedWithSystemBreakpoint(t *testing.T) {
	asst := Message{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "a"}}}
	var msgs []Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs, UserText("u"), asst)
	}
	msgs = append(msgs, UserText("last"))

	req := Request{System: []string{"sys"}, CacheBoundary: 1, Messages: msgs, CacheRecent: 10}
	_, total := cacheMarkers(t, sendCacheRequest(t, req))
	if total != MaxCacheBreakpoints {
		t.Errorf("total markers = %d, want exactly %d", total, MaxCacheBreakpoints)
	}
}

func TestAnthropicCacheRecentAfterMerge(t *testing.T) {
	msgs := []Message{
		UserText("first"),
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "a"}}},
		toolResultMessage("t1"),
		UserText("second"),
	}
	got, total := cacheMarkers(t, sendCacheRequest(t, Request{Messages: msgs, CacheRecent: 1}))
	want := []bool{false, false, true}
	if !equalBools(got, want) || total != 1 {
		t.Errorf("markers = %v (total %d), want %v (total 1)", got, total, want)
	}
}

func TestAnthropicCacheRecentZeroMarksNothing(t *testing.T) {
	_, total := cacheMarkers(t, sendCacheRequest(t, Request{Messages: []Message{UserText("hi")}}))
	if total != 0 {
		t.Errorf("total markers = %d, want 0", total)
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
