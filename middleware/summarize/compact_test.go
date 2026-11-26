package summarize

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func TestSummaryInputBounded(t *testing.T) {
	// About 2.4 MB rendered: text turns and tool turns of ~2000 chars each.
	big := strings.Repeat("y", 1990)
	var msgs []llm.Message
	for i := 0; len(msgs) < 1200; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			llm.UserText(fmt.Sprintf("Q%d %s", i, big)), assistantCalls(id),
			userResults(big, id), assistantText(fmt.Sprintf("R%d %s", i, big)))
	}
	msgs = append(msgs, llm.UserText("LAST"))

	main := llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{compactCall("k1")}}, llmtest.Turn{Text: "done"})
	summarizer := llmtest.New(llmtest.Turn{Text: "SUMMARY"})
	runAgent(t, main, testutil.Memory(t), Config{Provider: summarizer, TriggerFraction: noAutoTrigger}, msgs)

	input := summarizerInput(t, summarizer, 0)
	const prefix = "Previous summary:\n\n\nNew conversation:\n"
	if !strings.HasPrefix(input, prefix) {
		t.Fatalf("input starts with %.60q", input)
	}
	rendered := strings.TrimPrefix(input, prefix)
	header, body, ok := strings.Cut(rendered, "\n\n")
	if !ok || !strings.HasPrefix(header, "[earlier conversation omitted: ") || !strings.HasSuffix(header, " messages]") {
		t.Fatalf("rendered conversation starts with %.80q, want the omission line", rendered)
	}
	if len(body) > maxSummaryInputChars {
		t.Fatalf("summarizer got %d chars of conversation, want at most %d", len(body), maxSummaryInputChars)
	}
	if len(body) < maxSummaryInputChars/2 {
		t.Fatalf("summarizer got only %d chars of conversation", len(body))
	}
	// The newest messages are kept; the oldest are not.
	if strings.Contains(body, "Q0 ") || !strings.Contains(body, "R290 ") {
		t.Fatal("the kept part is not the newest part of the conversation")
	}
	if strings.HasPrefix(body, "## user\ntool_result") {
		t.Fatal("the kept part starts with a tool result cut off from its call")
	}
}

func TestBoundedRenderKeepsToolPairs(t *testing.T) {
	msgs := []llm.Message{
		llm.UserText("first"), assistantCalls("c1"), userResults("out", "c1"), assistantText("last"),
	}
	// msgs[2:] fits exactly, but starting at the tool result would cut it
	// from its call, so the kept part starts after it.
	limit := len(render(msgs[2:], DefaultMaxBlockChars))
	got := boundedRender(msgs, limit)
	want := "[earlier conversation omitted: 3 messages]\n\n" + render(msgs[3:], DefaultMaxBlockChars)
	if got != want {
		t.Fatalf("boundedRender = %q, want %q", got, want)
	}

	if got := boundedRender(msgs, 1_000_000); got != render(msgs, DefaultMaxBlockChars) {
		t.Fatalf("a conversation within the bound changed: %q", got)
	}
}

func TestBoundedRenderCutsOversizedMessage(t *testing.T) {
	m := llm.Message{Role: llm.RoleUser}
	for i := range 50 {
		m.Content = append(m.Content, llm.TextBlock(fmt.Sprintf("block%d %s", i, strings.Repeat("é", 500))))
	}
	const limit = 5000
	got := boundedRender([]llm.Message{llm.UserText("old"), m}, limit)
	header, body, _ := strings.Cut(got, "\n\n")
	if header != "[earlier conversation omitted: 1 messages]" {
		t.Fatalf("header = %q", header)
	}
	if len(body) > limit || !strings.HasSuffix(body, strings.Repeat("é", 500)) || !strings.Contains(body, "block49") {
		t.Fatalf("body has %d chars, ends %q", len(body), body[max(0, len(body)-20):])
	}
	if !strings.HasPrefix(body, "…") {
		t.Fatalf("a cut message does not say so: %.20q", body)
	}
}

func TestCutFrontTinyLimit(t *testing.T) {
	for limit := range len("…") {
		if got := cutFront("abcdef", limit); got != "" {
			t.Fatalf("cutFront(limit %d) = %q", limit, got)
		}
	}
	if got := cutFront("abcdef", 4); got != "…f" {
		t.Fatalf("cutFront(limit 4) = %q", got)
	}
}
