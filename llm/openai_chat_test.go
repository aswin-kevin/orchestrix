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

func newTestOpenAI(t *testing.T, baseURL string, mutate ...func(*Config)) *openAIProvider {
	t.Helper()
	cfg := Config{
		Format:        FormatOpenAI,
		BaseURL:       baseURL,
		APIKey:        "key-1",
		Model:         "chat-test",
		MaxRetries:    -1,
		RetryInterval: time.Millisecond,
		Headers:       map[string]string{"X-Extra": "yes"},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := newOpenAI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// chunks renders data payloads as an SSE body ending in [DONE].
func chunks(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		sb.WriteString("data: " + p + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

const openAIHelloStream = `data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

func collectOpenAI(t *testing.T, p *openAIProvider, req Request) *Response {
	t.Helper()
	if len(req.Messages) == 0 {
		req.Messages = []Message{UserText("x")}
	}
	resp, err := Collect(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestOpenAIChatRequestShape(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", openAIHelloStream)
	p := newTestOpenAI(t, srv.URL+"/v1/")

	req := Request{
		System: []string{"first", "second"},
		Messages: []Message{
			UserText("run it"),
			{Role: RoleAssistant, Content: []Block{
				{Type: BlockThinking, Thinking: "hmm", Signature: "sig"},
				TextBlock("ok"),
				{
					Type: BlockToolUse, ID: "call_1", Name: "ls", Input: json.RawMessage(`{"a":1}`),
					ProviderData: map[string]string{openAIExtraContentKey: `{"google":{"thought_signature":"abc"}}`},
				},
			}},
			{Role: RoleUser, Content: []Block{
				{Type: BlockToolResult, ToolUseID: "call_1", Content: []Block{
					TextBlock("file.txt"), {Type: BlockImage},
				}},
				TextBlock("thanks"),
			}},
		},
		Tools: []ToolSchema{{Name: "ls", Description: "list", InputSchema: map[string]any{"type": "object"}}},
		Stop:  []string{"END"},
	}
	collectOpenAI(t, p, req)

	if srv.path != "/v1/chat/completions" {
		t.Errorf("path = %q", srv.path)
	}
	for k, want := range map[string]string{
		"Authorization": "Bearer key-1", "Content-Type": "application/json", "X-Extra": "yes",
	} {
		if got := srv.headers.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if srv.body["stream"] != true || srv.body["model"] != "chat-test" {
		t.Errorf("stream/model = %v/%v", srv.body["stream"], srv.body["model"])
	}
	if opts := srv.body["stream_options"].(map[string]any); opts["include_usage"] != true {
		t.Errorf("stream_options = %v", opts)
	}

	msgs := srv.body["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages = %v", msgs)
	}
	system := msgs[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "first\n\nsecond" {
		t.Errorf("system = %v", system)
	}
	assistant := msgs[2].(map[string]any)
	if assistant["content"] != "ok" {
		t.Errorf("assistant = %v", assistant)
	}
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" || fn["name"] != "ls" || fn["arguments"] != `{"a":1}` {
		t.Errorf("tool call = %v", call)
	}
	extra, _ := json.Marshal(call["extra_content"])
	if string(extra) != `{"google":{"thought_signature":"abc"}}` {
		t.Errorf("extra_content = %s", extra)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "file.txt\n[image omitted]" {
		t.Errorf("tool message = %v", tool)
	}
	if user := msgs[4].(map[string]any); user["role"] != "user" || user["content"] != "thanks" {
		t.Errorf("trailing user = %v", user)
	}

	raw, _ := json.Marshal(srv.body)
	for _, leaked := range []string{"provider_data", "thinking", "hmm"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("%q leaked into body: %s", leaked, raw)
		}
	}
	if srv.body["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v", srv.body["max_tokens"])
	}
	toolDef := srv.body["tools"].([]any)[0].(map[string]any)
	if toolDef["type"] != "function" || toolDef["function"].(map[string]any)["name"] != "ls" {
		t.Errorf("tool def = %v", toolDef)
	}
}

func TestOpenAIChatImageContentParts(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", openAIHelloStream)
	p := newTestOpenAI(t, srv.URL)
	collectOpenAI(t, p, Request{Messages: []Message{{Role: RoleUser, Content: []Block{
		TextBlock("look"),
		{Type: BlockImage, Source: &Source{Type: "base64", MediaType: "image/png", Data: "AAAA"}},
		{Type: BlockImage, Source: &Source{Type: "url", URL: "https://example.com/a.png"}},
	}}}})

	parts := srv.body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %v", parts)
	}
	urls := []string{
		parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string),
		parts[2].(map[string]any)["image_url"].(map[string]any)["url"].(string),
	}
	if urls[0] != "data:image/png;base64,AAAA" || urls[1] != "https://example.com/a.png" {
		t.Errorf("urls = %v", urls)
	}
}

func TestOpenAIChatOptionalRequestFields(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", openAIHelloStream)
	p := newTestOpenAI(t, srv.URL, func(c *Config) {
		c.ReasoningEffort = "high"
		c.Thinking = "enabled"
	})
	collectOpenAI(t, p, Request{})

	if srv.body["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", srv.body["reasoning_effort"])
	}
	if th := srv.body["thinking"].(map[string]any); th["type"] != "enabled" {
		t.Errorf("thinking = %v", th)
	}

	plain := newSSEServer(t, 200, "text/event-stream", openAIHelloStream)
	collectOpenAI(t, newTestOpenAI(t, plain.URL), Request{})
	for _, key := range []string{"reasoning_effort", "thinking", "tools", "stop", "temperature"} {
		if _, ok := plain.body[key]; ok {
			t.Errorf("unset %s was sent", key)
		}
	}
}

func TestOpenAIChatMaxCompletionTokensField(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", openAIHelloStream)
	p := newTestOpenAI(t, srv.URL, func(c *Config) { c.MaxTokensField = "max_completion_tokens" })
	collectOpenAI(t, p, Request{MaxTokens: 123})

	if srv.body["max_completion_tokens"] != float64(123) {
		t.Errorf("max_completion_tokens = %v", srv.body["max_completion_tokens"])
	}
	if _, ok := srv.body["max_tokens"]; ok {
		t.Error("max_tokens must not be sent alongside max_completion_tokens")
	}
}

func TestOpenAIChatStreamParallelToolCalls(t *testing.T) {
	stream := chunks(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"write","arguments":"{\"p\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"f\":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

	uses := resp.Message.ToolUses()
	if len(uses) != 2 {
		t.Fatalf("tool uses = %+v", resp.Message.Content)
	}
	if uses[0].ID != "call_a" || uses[0].Name != "read" || string(uses[0].Input) != `{"f":1}` {
		t.Errorf("first = %+v", uses[0])
	}
	if uses[1].ID != "call_b" || uses[1].Name != "write" || string(uses[1].Input) != `{"p":2}` {
		t.Errorf("second = %+v", uses[1])
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
}

func TestOpenAIChatToolCallExtraContentAndMissingID(t *testing.T) {
	stream := chunks(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{}"},"extra_content":{"k":"v"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

	use := resp.Message.ToolUses()[0]
	if use.ProviderData[openAIExtraContentKey] != `{"k":"v"}` {
		t.Errorf("provider data = %v", use.ProviderData)
	}
	if !strings.HasPrefix(use.ID, "call_") || !strings.HasSuffix(use.ID, "_0") || len(use.ID) != len("call_12345678_0") {
		t.Errorf("synthesised id = %q", use.ID)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop = %q, want tool_use even though the server said stop", resp.StopReason)
	}
}

func TestOpenAIChatReasoningField(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		t.Run(field, func(t *testing.T) {
			stream := chunks(
				`{"choices":[{"delta":{"`+field+`":"think"}}]}`,
				`{"choices":[{"delta":{"content":"answer"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			)
			srv := newSSEServer(t, 200, "text/event-stream", stream)
			resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

			blocks := resp.Message.Content
			if len(blocks) != 2 || blocks[0].Type != BlockThinking || blocks[0].Thinking != "think" ||
				blocks[1].Type != BlockText || blocks[1].Text != "answer" {
				t.Errorf("content = %+v", blocks)
			}
		})
	}
}

func TestOpenAIChatUsageCached(t *testing.T) {
	stream := chunks(
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":60}}}`,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

	if want := (Usage{Input: 40, Output: 7, CacheRead: 60}); resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
	if resp.StopReason != StopMaxTokens {
		t.Errorf("stop = %q", resp.StopReason)
	}
}

func TestOpenAIChatRepeatedUsageNotDoubleCounted(t *testing.T) {
	stream := chunks(
		`{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`,
		`{"choices":[{"delta":{"content":"b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

	if want := (Usage{Input: 10, Output: 2}); resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestOpenAIChatFinishWithoutDone(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

	if resp.Message.Text() != "hi" || resp.StopReason != StopEndTurn {
		t.Errorf("resp = %+v", resp)
	}
}

func TestOpenAIChatMidStreamDropNotRetried(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	p := newTestOpenAI(t, srv.URL, func(c *Config) { c.MaxRetries = 3 })

	if _, err := Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}}); err == nil {
		t.Fatal("expected error for truncated stream")
	}
	if got := srv.hits.Load(); got != 1 {
		t.Errorf("server hit %d times, want 1", got)
	}
}

func TestOpenAIChatStreamErrorChunk(t *testing.T) {
	stream := chunks(`{"error":{"message":"maximum context length exceeded"}}`)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	_, err := Collect(context.Background(), newTestOpenAI(t, srv.URL), Request{Messages: []Message{UserText("x")}})

	if !IsContextOverflow(err) {
		t.Errorf("err = %v, want context overflow", err)
	}
}

// newSequenceServer answers successive requests with successive replies,
// repeating the last one when they run out.
func newSequenceServer(t *testing.T, replies ...string) (srv *httptest.Server, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1)) - 1
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, replies[min(n, len(replies)-1)])
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

const (
	openAIEmptyStream = `data: {"choices":[{"delta":{"role":"assistant","content":""}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	openAIThinkingOnlyStream = `data: {"choices":[{"delta":{"reasoning_content":"hm"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
)

func TestOpenAIChatEmptyResponseRetried(t *testing.T) {
	for name, empty := range map[string]string{"empty": openAIEmptyStream, "thinking only": openAIThinkingOnlyStream} {
		t.Run(name, func(t *testing.T) {
			srv, hits := newSequenceServer(t, empty, openAIHelloStream)
			resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

			if got := hits.Load(); got != 2 {
				t.Errorf("hits = %d, want 2", got)
			}
			if resp.Message.Text() != "hi" {
				t.Errorf("text = %q", resp.Message.Text())
			}
			for _, b := range resp.Message.Content {
				if b.Type == BlockThinking {
					t.Error("thinking from the discarded attempt leaked through")
				}
			}
		})
	}
}

func TestOpenAIChatEmptyResponseRetriesExhausted(t *testing.T) {
	srv, hits := newSequenceServer(t, openAIThinkingOnlyStream)
	p := newTestOpenAI(t, srv.URL, func(c *Config) { c.EmptyResponseRetries = 1 })
	resp := collectOpenAI(t, p, Request{})

	if got := hits.Load(); got != 2 {
		t.Errorf("hits = %d, want 2 (one retry)", got)
	}
	if resp.Message.Text() != "" || resp.StopReason != StopEndTurn {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Thinking != "hm" {
		t.Errorf("last attempt's output not returned: %+v", resp.Message.Content)
	}
}

func TestOpenAIChatEmptyResponseRetryDisabled(t *testing.T) {
	srv, hits := newSequenceServer(t, openAIEmptyStream, openAIHelloStream)
	p := newTestOpenAI(t, srv.URL, func(c *Config) { c.EmptyResponseRetries = -1 })
	collectOpenAI(t, p, Request{})

	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
}

func TestOpenAIChatNoRetryOnceContentYielded(t *testing.T) {
	// Content arrives, then the stream drops: the consumer has seen text, so
	// the error must surface instead of a silent replay.
	srv, hits := newSequenceServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n", openAIHelloStream)
	_, err := Collect(context.Background(), newTestOpenAI(t, srv.URL), Request{Messages: []Message{UserText("x")}})

	if err == nil || hits.Load() != 1 {
		t.Errorf("err = %v, hits = %d", err, hits.Load())
	}
}

const openAIFirstDelta = `data: {"choices":[{"delta":{"content":"a"}}]}

`

func TestOpenAIChatStreamConsumerBreakClosesConnection(t *testing.T) {
	url, gone := newBlockingServer(t, openAIFirstDelta)
	p := newTestOpenAI(t, url)

	for _, err := range p.Stream(context.Background(), Request{Messages: []Message{UserText("x")}}) {
		if err != nil {
			t.Fatal(err)
		}
		// The first event released is the buffered message_start, which
		// arrives together with the first text.
		break
	}
	waitClosed(t, gone)
}

func TestOpenAIChatStreamContextCancelClosesConnection(t *testing.T) {
	url, gone := newBlockingServer(t, openAIFirstDelta)
	p := newTestOpenAI(t, url)
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

func TestOpenAIChatHistoryMergesAndDropsEmpty(t *testing.T) {
	msgs := toOpenAIMessages(nil, []Message{
		UserText("one"),
		{Role: RoleAssistant, Content: []Block{TextBlock("a")}},
		{Role: RoleUser, Content: []Block{TextBlock("")}},
		{Role: RoleAssistant, Content: []Block{
			{Type: BlockThinking, Thinking: "t"},
			{Type: BlockToolUse, ID: "c", Name: "n"},
		}},
		{Role: RoleAssistant, Content: []Block{TextBlock("")}},
	})
	if len(msgs) != 2 {
		t.Fatalf("messages = %+v", msgs)
	}
	merged := msgs[1]
	if merged.Role != "assistant" || merged.Content != "a" || len(merged.ToolCalls) != 1 {
		t.Errorf("merged = %+v", merged)
	}
	if args := merged.ToolCalls[0].Function.Arguments; args != "{}" {
		t.Errorf("empty input arguments = %q, want {}", args)
	}
}

func TestOpenAIChatToolDefaultSchema(t *testing.T) {
	tools := toOpenAITools([]ToolSchema{{Name: "noargs"}})
	if tools[0].Function.Parameters["type"] != "object" {
		t.Errorf("schema = %v", tools[0].Function.Parameters)
	}
}

func TestOpenAIChatNoAuthHeaderWithoutKey(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", openAIHelloStream)
	collectOpenAI(t, newTestOpenAI(t, srv.URL, func(c *Config) { c.APIKey = "" }), Request{})

	if got := srv.headers.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestParallelToolCallsWithoutIndex(t *testing.T) {
	stream := chunks(
		`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"one","arguments":"{\"p\":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"two","arguments":"{\"q\":2}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

	uses := resp.Message.ToolUses()
	if len(uses) != 2 {
		t.Fatalf("tool uses = %+v", resp.Message.Content)
	}
	if uses[0].ID != "a" || uses[0].Name != "one" || string(uses[0].Input) != `{"p":1}` {
		t.Errorf("first = %+v", uses[0])
	}
	if uses[1].ID != "b" || uses[1].Name != "two" || string(uses[1].Input) != `{"q":2}` {
		t.Errorf("second = %+v", uses[1])
	}
}

func TestParallelToolCallsWithIndexUnchanged(t *testing.T) {
	stream := chunks(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"x","function":{"name":"one","arguments":"{\"a\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"y","function":{"name":"two","arguments":"{\"b\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

	uses := resp.Message.ToolUses()
	if len(uses) != 2 {
		t.Fatalf("tool uses = %+v", resp.Message.Content)
	}
	if uses[0].ID != "x" || string(uses[0].Input) != `{"a":1}` {
		t.Errorf("first = %+v", uses[0])
	}
	if uses[1].ID != "y" || string(uses[1].Input) != `{"b":2}` {
		t.Errorf("second = %+v", uses[1])
	}
}

func TestSyntheticToolIDsUniqueAcrossResponses(t *testing.T) {
	stream := chunks(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	p := newTestOpenAI(t, srv.URL)

	first := collectOpenAI(t, p, Request{}).Message.ToolUses()[0].ID
	second := collectOpenAI(t, p, Request{}).Message.ToolUses()[0].ID
	if first == "" || first == second {
		t.Errorf("synthetic ids %q and %q must be non-empty and distinct", first, second)
	}
}

func TestDoneWithoutFinishReason(t *testing.T) {
	for name, tc := range map[string]struct {
		payload string
		want    string
	}{
		"text": {`{"choices":[{"delta":{"content":"hi"}}]}`, StopEndTurn},
		"tool": {`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":"{}"}}]}}]}`, StopToolUse},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newSSEServer(t, 200, "text/event-stream", chunks(tc.payload))
			resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})
			if resp.StopReason != tc.want {
				t.Errorf("stop = %q, want %q", resp.StopReason, tc.want)
			}
		})
	}
}

func TestEmptyReplyLengthNotRetried(t *testing.T) {
	stream := chunks(
		`{"choices":[{"delta":{"reasoning_content":"long thought"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
	)
	srv, hits := newSequenceServer(t, stream, openAIHelloStream)
	resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
	if resp.StopReason != StopMaxTokens {
		t.Errorf("stop = %q, want %q", resp.StopReason, StopMaxTokens)
	}
}

func TestEmptyReplyRetryUsageSummed(t *testing.T) {
	empty := chunks(
		`{"choices":[{"delta":{"content":""},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`,
	)
	text := chunks(
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
	)
	srv, hits := newSequenceServer(t, empty, empty, text)
	resp := collectOpenAI(t, newTestOpenAI(t, srv.URL), Request{})

	if got := hits.Load(); got != 3 {
		t.Errorf("hits = %d, want 3", got)
	}
	if want := (Usage{Input: 30, Output: 7}); resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestHeadersCopied(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", openAIHelloStream)
	headers := map[string]string{"X-Extra": "yes"}
	p, err := New(Config{Format: FormatOpenAI, BaseURL: srv.URL, Model: "m", MaxRetries: -1, Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	headers["X-Extra"] = "mutated"
	headers["X-Added"] = "late"

	if _, err := Collect(context.Background(), p, Request{Messages: []Message{UserText("x")}}); err != nil {
		t.Fatal(err)
	}
	if got := srv.headers.Get("X-Extra"); got != "yes" {
		t.Errorf("X-Extra = %q, want yes", got)
	}
	if got := srv.headers.Get("X-Added"); got != "" {
		t.Errorf("X-Added = %q, want unset", got)
	}
}

func TestOpenAIDefaultsToMaxCompletionTokens(t *testing.T) {
	for _, tc := range []struct {
		baseURL, field string
		want           string
	}{
		{"https://api.openai.com/v1", "", "max_completion_tokens"},
		{"https://api.openai.com/v1", "max_tokens", "max_tokens"},
		{"https://llm.example.com/v1", "", "max_tokens"},
	} {
		p := newTestOpenAI(t, tc.baseURL, func(c *Config) { c.MaxTokensField = tc.field })
		raw, err := json.Marshal(p.buildRequest(Request{MaxTokens: 50}))
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		other := "max_tokens"
		if tc.want == other {
			other = "max_completion_tokens"
		}
		if body[tc.want] != float64(50) {
			t.Errorf("%s field=%q: %s = %v", tc.baseURL, tc.field, tc.want, body[tc.want])
		}
		if _, ok := body[other]; ok {
			t.Errorf("%s field=%q: %s must not be sent", tc.baseURL, tc.field, other)
		}
	}
}
