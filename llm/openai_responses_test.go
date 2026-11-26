package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newTestResponses(t *testing.T, baseURL string, mutate ...func(*Config)) *responsesProvider {
	t.Helper()
	cfg := Config{
		Format:        FormatOpenAIResponses,
		BaseURL:       baseURL,
		APIKey:        "key-1",
		Model:         "resp-test",
		MaxRetries:    NoRetries,
		RetryInterval: time.Millisecond,
		Headers:       map[string]string{"X-Extra": "yes"},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := newResponses(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// responsesBody renders event payloads as an SSE body. The event kind is read
// from each payload's "type" field, as the adapter does.
func responsesBody(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		sb.WriteString("data: " + p + "\n\n")
	}
	return sb.String()
}

const (
	responsesCompleted = `{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}}`
	responsesTextDelta = `{"type":"response.output_text.delta","output_index":0,"delta":"hi"}`
)

func collectResponses(t *testing.T, p *responsesProvider, req Request) *Response {
	t.Helper()
	if len(req.Messages) == 0 {
		req.Messages = []Message{UserText("x")}
	}
	resp, err := Collect(t.Context(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestResponsesRequestShape(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", responsesBody(responsesTextDelta, responsesCompleted))
	p := newTestResponses(t, srv.URL+"/v1/", func(c *Config) { c.ReasoningEffort = "low" })

	temp := 0.5
	req := Request{
		System: []string{"first", "", "second"},
		Messages: []Message{
			{Role: RoleUser, Content: []Block{
				TextBlock("look"), TextBlock(""),
				{Type: BlockImage, Source: &Source{Type: "base64", MediaType: "image/png", Data: "QUJD"}},
				{Type: BlockImage, Source: &Source{Type: "url", URL: "https://x/y.png"}},
			}},
			{Role: RoleAssistant, Content: []Block{
				{Type: BlockThinking, Thinking: "hmm", Signature: "enc", ProviderData: map[string]string{responsesReasoningIDKey: "rs_1"}},
				{Type: BlockThinking, Thinking: "unsigned"},
				TextBlock("ok"),
				{Type: BlockToolUse, ID: "call_1", Name: "ls", Input: json.RawMessage(`{"a":1}`)},
			}},
			{Role: RoleUser, Content: []Block{
				{Type: BlockToolResult, ToolUseID: "call_1", Content: []Block{TextBlock("file.txt"), {Type: BlockImage}}},
				TextBlock("thanks"),
			}},
		},
		Tools:       []ToolSchema{{Name: "ls", Description: "list", InputSchema: map[string]any{"type": "object", "x": 1}}, {Name: "noargs"}},
		MaxTokens:   77,
		Temperature: &temp,
	}
	collectResponses(t, p, req)

	if srv.path != "/v1/responses" {
		t.Errorf("path = %q", srv.path)
	}
	for k, want := range map[string]string{
		"Authorization": "Bearer key-1", "Content-Type": "application/json", "X-Extra": "yes",
	} {
		if got := srv.headers.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	b := srv.body
	if b["stream"] != true || b["store"] != false || b["model"] != "resp-test" ||
		b["instructions"] != "first\n\nsecond" || b["max_output_tokens"] != 77.0 || b["temperature"] != 0.5 {
		t.Errorf("top-level fields = %v", b)
	}
	if inc, _ := json.Marshal(b["include"]); string(inc) != `["reasoning.encrypted_content"]` {
		t.Errorf("include = %s", inc)
	}
	if r, _ := json.Marshal(b["reasoning"]); string(r) != `{"effort":"low"}` {
		t.Errorf("reasoning = %s", r)
	}
	tools, _ := json.Marshal(b["tools"])
	wantTools := `[{"description":"list","name":"ls","parameters":{"type":"object","x":1},"type":"function"},{"name":"noargs","parameters":{"type":"object"},"type":"function"}]`
	if string(tools) != wantTools {
		t.Errorf("tools = %s", tools)
	}

	input, _ := json.Marshal(b["input"])
	wantInput := `[` +
		`{"content":[{"text":"look","type":"input_text"},` +
		`{"image_url":"data:image/png;base64,QUJD","type":"input_image"},` +
		`{"image_url":"https://x/y.png","type":"input_image"}],"role":"user","type":"message"},` +
		`{"encrypted_content":"enc","id":"rs_1","summary":[],"type":"reasoning"},` +
		`{"content":[{"text":"ok","type":"output_text"}],"role":"assistant","type":"message"},` +
		`{"arguments":"{\"a\":1}","call_id":"call_1","name":"ls","type":"function_call"},` +
		`{"call_id":"call_1","output":"file.txt\n[image omitted]","type":"function_call_output"},` +
		`{"content":[{"text":"thanks","type":"input_text"}],"role":"user","type":"message"}]`
	if string(input) != wantInput {
		t.Errorf("input =\n%s\nwant\n%s", input, wantInput)
	}
}

func TestResponsesOptionalFieldsOmitted(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", responsesBody(responsesTextDelta, responsesCompleted))
	collectResponses(t, newTestResponses(t, srv.URL), Request{})

	for _, k := range []string{"instructions", "reasoning", "tools", "temperature"} {
		if _, ok := srv.body[k]; ok {
			t.Errorf("%s present: %v", k, srv.body[k])
		}
	}
	if srv.body["max_output_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_output_tokens = %v", srv.body["max_output_tokens"])
	}
}

func TestResponsesNoAuthHeaderWithoutKey(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", responsesBody(responsesTextDelta, responsesCompleted))
	collectResponses(t, newTestResponses(t, srv.URL, func(c *Config) { c.APIKey = "" }), Request{})

	if got := srv.headers.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q", got)
	}
}

const responsesToolStream = `
data: {"type":"response.created","response":{}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_9","summary":[]}}

data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"think"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_9","encrypted_content":"ENC"}}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant"}}

data: {"type":"response.output_text.delta","output_index":1,"delta":"let me "}

data: {"type":"response.output_text.delta","output_index":1,"delta":"look"}

data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_7","name":"ls"}}

data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"path\""}

data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":":\".\"}"}

data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"call_7","name":"ls","arguments":"{\"path\":\".\"}"}}

data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}}

`

func TestResponsesStreamToolCallAndReasoningReplay(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", responsesToolStream)
	p := newTestResponses(t, srv.URL)

	resp := collectResponses(t, p, Request{})
	if resp.StopReason != StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if want := (Usage{Input: 6, Output: 2, CacheRead: 4}); resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
	content := resp.Message.Content
	if len(content) != 3 {
		t.Fatalf("content = %+v", content)
	}
	if th := content[0]; th.Type != BlockThinking || th.Thinking != "think" || th.Signature != "ENC" ||
		th.ProviderData[responsesReasoningIDKey] != "rs_9" {
		t.Errorf("thinking = %+v", th)
	}
	if content[1].Text != "let me look" {
		t.Errorf("text = %+v", content[1])
	}
	if tu := content[2]; tu.ID != "call_7" || tu.Name != "ls" || string(tu.Input) != `{"path":"."}` {
		t.Errorf("tool use = %+v", tu)
	}

	// Feeding the reply back must reproduce the reasoning and call items.
	history := []Message{UserText("go"), resp.Message, {Role: RoleUser, Content: []Block{ToolResultText("call_7", "a.go", false)}}}
	collectResponses(t, p, Request{Messages: history})
	input, _ := json.Marshal(srv.body["input"])
	for _, want := range []string{
		`{"encrypted_content":"ENC","id":"rs_9","summary":[],"type":"reasoning"}`,
		`{"arguments":"{\"path\":\".\"}","call_id":"call_7","name":"ls","type":"function_call"}`,
		`{"call_id":"call_7","output":"a.go","type":"function_call_output"}`,
	} {
		if !strings.Contains(string(input), want) {
			t.Errorf("input missing %s:\n%s", want, input)
		}
	}
}

func TestResponsesToolArgumentsFromDoneItem(t *testing.T) {
	stream := responsesBody(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"ls"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c","name":"ls","arguments":"{\"a\":1}"}}`,
		responsesCompleted,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectResponses(t, newTestResponses(t, srv.URL), Request{})

	if got := resp.Message.Content[0]; string(got.Input) != `{"a":1}` {
		t.Errorf("input = %s", got.Input)
	}
}

func TestResponsesTextEndTurn(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", responsesBody(responsesTextDelta, responsesCompleted))
	resp := collectResponses(t, newTestResponses(t, srv.URL), Request{})

	if resp.Message.Text() != "hi" || resp.StopReason != StopEndTurn {
		t.Errorf("resp = %+v", resp)
	}
}

func TestResponsesIncompleteMaxTokens(t *testing.T) {
	stream := responsesBody(
		responsesTextDelta,
		`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":3,"output_tokens":9}}}`,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectResponses(t, newTestResponses(t, srv.URL), Request{})

	if resp.StopReason != StopMaxTokens || resp.Message.Text() != "hi" {
		t.Errorf("resp = %+v", resp)
	}
	if want := (Usage{Input: 3, Output: 9}); resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestResponsesFailedEvent(t *testing.T) {
	stream := responsesBody(`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}}`)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	_, err := Collect(t.Context(), newTestResponses(t, srv.URL), Request{Messages: []Message{UserText("x")}})

	var apiErr *APIError
	if !errors.As(err, &apiErr) || !IsContextOverflow(err) {
		t.Errorf("err = %v, want *APIError with context overflow", err)
	}
}

func TestResponsesErrorEvent(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", responsesBody(`{"type":"error","code":"server_error","message":"boom"}`))
	_, err := Collect(t.Context(), newTestResponses(t, srv.URL), Request{Messages: []Message{UserText("x")}})

	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Body, "boom") {
		t.Errorf("err = %v, want *APIError carrying the message", err)
	}
}

func TestResponsesTruncatedStreamNotRetried(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", responsesBody(responsesTextDelta))
	p := newTestResponses(t, srv.URL, func(c *Config) { c.MaxRetries = 3 })

	if _, err := Collect(t.Context(), p, Request{Messages: []Message{UserText("x")}}); err == nil {
		t.Fatal("expected error for truncated stream")
	}
	if got := srv.hits.Load(); got != 1 {
		t.Errorf("server hit %d times, want 1", got)
	}
}

func TestResponsesHTTPErrorRetried(t *testing.T) {
	srv := newSSEServer(t, 503, "application/json", `{"error":"busy"}`)
	p := newTestResponses(t, srv.URL, func(c *Config) { c.MaxRetries = 2 })

	_, err := Collect(t.Context(), p, Request{Messages: []Message{UserText("x")}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 503 {
		t.Fatalf("err = %v", err)
	}
	if got := srv.hits.Load(); got != 3 {
		t.Errorf("server hit %d times, want 3", got)
	}
}

const responsesFirstDelta = `data: {"type":"response.output_text.delta","output_index":0,"delta":"a"}

`

func TestResponsesStreamConsumerBreakClosesConnection(t *testing.T) {
	url, gone := newBlockingServer(t, responsesFirstDelta)
	p := newTestResponses(t, url)

	for _, err := range p.Stream(t.Context(), Request{Messages: []Message{UserText("x")}}) {
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	waitClosed(t, gone)
}

func TestResponsesStreamContextCancelClosesConnection(t *testing.T) {
	url, gone := newBlockingServer(t, responsesFirstDelta)
	p := newTestResponses(t, url)
	ctx, cancel := context.WithCancel(t.Context())
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

func TestResponsesRawReasoningTextDelta(t *testing.T) {
	stream := responsesBody(
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"raw "}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"thought"}`,
		responsesCompleted,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectResponses(t, newTestResponses(t, srv.URL), Request{})

	if got := resp.Message.Content[0]; got.Type != BlockThinking || got.Thinking != "raw thought" {
		t.Errorf("block = %+v", got)
	}
}

func TestResponsesFunctionCallOnlyOnDoneItem(t *testing.T) {
	stream := responsesBody(
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c9","name":"ls","arguments":"{\"a\":1}"}}`,
		responsesCompleted,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectResponses(t, newTestResponses(t, srv.URL), Request{})

	if resp.StopReason != StopToolUse {
		t.Errorf("stop = %q, want tool_use", resp.StopReason)
	}
	if tu := resp.Message.ToolUses(); len(tu) != 1 || tu[0].ID != "c9" || tu[0].Name != "ls" || string(tu[0].Input) != `{"a":1}` {
		t.Errorf("tool uses = %+v", tu)
	}
}

func TestResponsesStampsReasoning(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", responsesToolStream)
	p := newTestResponses(t, srv.URL)

	resp := collectResponses(t, p, Request{})
	if got := resp.Message.Content[0].ProviderData[formatKey]; got != string(FormatOpenAIResponses) {
		t.Errorf("reasoning stamp = %q, want %q", got, FormatOpenAIResponses)
	}
}

func TestResponsesForeignThinkingNotReplayed(t *testing.T) {
	srv := newSSEServer(t, 200, "text/event-stream", responsesBody(responsesTextDelta, responsesCompleted))
	p := newTestResponses(t, srv.URL)
	stamped := func(format Format, sig string) Block {
		return Block{Type: BlockThinking, Signature: sig, ProviderData: map[string]string{formatKey: string(format)}}
	}

	history := []Message{UserText("q"), {Role: RoleAssistant, Content: []Block{
		stamped(FormatAnthropic, "FOREIGN"),
		stamped(FormatOpenAIResponses, "OWN"),
		{Type: BlockThinking, Signature: "LEGACY"},
		TextBlock("a"),
	}}, UserText("again")}
	collectResponses(t, p, Request{Messages: history})
	input, _ := json.Marshal(srv.body["input"])
	if strings.Contains(string(input), "FOREIGN") {
		t.Errorf("anthropic thinking replayed: %s", input)
	}
	for _, want := range []string{"OWN", "LEGACY"} {
		if !strings.Contains(string(input), `"encrypted_content":"`+want+`"`) {
			t.Errorf("input missing %s reasoning: %s", want, input)
		}
	}
}

func TestResponsesResponseModelAndID(t *testing.T) {
	body := responsesBody(
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"resp-wire","output":[]}}`,
		responsesTextDelta,
		`{"type":"response.completed","sequence_number":2,"response":{"id":"resp_1","object":"response","status":"completed","model":"resp-wire","usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	srv := newSSEServer(t, 200, "text/event-stream", body)
	resp := collectResponses(t, newTestResponses(t, srv.URL), Request{})
	if resp.Model != "resp-wire" || resp.ID != "resp_1" {
		t.Errorf("Model, ID = %q, %q", resp.Model, resp.ID)
	}
}

func TestResponsesEarlyStreamErrorYieldsOnlyTheError(t *testing.T) {
	checkEarlyStreamError(t, func(t *testing.T, u string) Provider { return newTestResponses(t, u) })
}

func TestResponsesFirstEventErrorEndsStream(t *testing.T) {
	srv := newSSEServer(t, http.StatusOK, "text/event-stream",
		"event: error\ndata: {\"type\":\"error\",\"message\":\"overloaded\"}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n")
	checkFirstEventError(t, newTestResponses(t, srv.URL))
}

// Some servers reuse an output_index for a later item, against the spec;
// each announced item must still get its own block.
func TestResponsesReusedOutputIndexStartsNewBlock(t *testing.T) {
	stream := responsesBody(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"done"}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_2"}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"why"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_2","encrypted_content":"ENC"}}`,
		responsesCompleted,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectResponses(t, newTestResponses(t, srv.URL), Request{})

	content := resp.Message.Content
	if len(content) != 2 {
		t.Fatalf("content = %+v, want a text and a thinking block", content)
	}
	if content[0].Type != BlockText || content[0].Text != "done" {
		t.Errorf("block 0 = %+v, want text %q", content[0], "done")
	}
	if th := content[1]; th.Type != BlockThinking || th.Thinking != "why" || th.Signature != "ENC" ||
		th.ProviderData[responsesReasoningIDKey] != "rs_2" {
		t.Errorf("block 1 = %+v, want thinking %q signed ENC from rs_2", th, "why")
	}
}

// A delta of another kind at an output_index, with no item announced, opens a
// new block instead of failing the stream.
func TestResponsesKindChangeWithoutAddedStartsNewBlock(t *testing.T) {
	stream := responsesBody(
		`{"type":"response.output_text.delta","output_index":0,"delta":"answer"}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"thought"}`,
		responsesCompleted,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectResponses(t, newTestResponses(t, srv.URL), Request{})

	content := resp.Message.Content
	if len(content) != 2 || content[0].Text != "answer" || content[1].Thinking != "thought" {
		t.Errorf("content = %+v, want text %q then thinking %q", content, "answer", "thought")
	}
}

// An item announced twice under the same id stays one block.
func TestResponsesRepeatedItemAnnouncementKeepsBlock(t *testing.T) {
	stream := responsesBody(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"one "}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"block"}`,
		responsesCompleted,
	)
	srv := newSSEServer(t, 200, "text/event-stream", stream)
	resp := collectResponses(t, newTestResponses(t, srv.URL), Request{})

	if content := resp.Message.Content; len(content) != 1 || content[0].Text != "one block" {
		t.Errorf("content = %+v, want one text block %q", content, "one block")
	}
}
