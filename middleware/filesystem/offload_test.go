package filesystem

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

func readAll(t *testing.T, b backend.Backend, path string) string {
	t.Helper()
	res, err := b.Read(context.Background(), path, 0, 1_000_000)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Join(res.Lines, "\n")
}

// wrapToolWith runs the middleware's WrapTool around a handler returning res.
func wrapToolWith(t *testing.T, cfg Config, name, id string, res tool.Result) tool.Result {
	t.Helper()
	call := &agent.ToolCall{ID: id, Name: name, State: state.New(), Call: &tool.Call{Backend: cfg.Backend}}
	out, err := New(cfg).(agent.ToolWrapper).WrapTool(context.Background(), call, func(context.Context, *agent.ToolCall) (tool.Result, error) {
		return res, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func bigOutput() string {
	return "first line\n" + strings.Repeat("x", 100_000)
}

func TestLargeToolResultOffloaded(t *testing.T) {
	b := memory.New()
	original := bigOutput()
	out := wrapToolWith(t, Config{Backend: b}, "web_fetch", "call_1", tool.Text(original))

	if got := readAll(t, b, "/large_tool_results/call_1.txt"); got != original {
		t.Fatalf("stored file differs from original (len %d vs %d)", len(got), len(original))
	}
	text := out.Flatten()
	for _, want := range []string{"first line", "/large_tool_results/call_1.txt", "Output too large (100011 chars)"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q: %.300s", want, text)
		}
	}
	if len(text) > 5000 {
		t.Errorf("replacement still large: %d bytes", len(text))
	}
}

func TestOwnToolsNotOffloaded(t *testing.T) {
	b := memory.New()
	res := tool.Text(bigOutput())
	out := wrapToolWith(t, Config{Backend: b}, ToolReadFile, "call_1", res)
	if out.Flatten() != res.Flatten() {
		t.Fatal("read_file result was offloaded")
	}
	if _, err := b.Read(context.Background(), "/large_tool_results/call_1.txt", 0, 1); err == nil {
		t.Fatal("file written for own tool")
	}
}

func TestSmallResultAndNoBackendPassThrough(t *testing.T) {
	b := memory.New()
	if out := wrapToolWith(t, Config{Backend: b}, "x", "c", tool.Text("small")); out.Flatten() != "small" {
		t.Fatalf("small result changed: %q", out.Flatten())
	}
	big := tool.Text(bigOutput())
	if out := wrapToolWith(t, Config{}, "x", "c", big); out.Flatten() != big.Flatten() {
		t.Fatal("result changed without a backend")
	}
}

func TestOffloadKeepsErrorFlagImagesAndSanitizesID(t *testing.T) {
	b := memory.New()
	res := tool.Errorf("%s", bigOutput())
	res.Content = append(res.Content, llm.Block{Type: llm.BlockImage, Source: &llm.Source{Type: "base64"}})
	out := wrapToolWith(t, Config{Backend: b}, "x", "../a b/c", res)

	if !out.IsError || len(out.Content) != 2 || out.Content[1].Type != llm.BlockImage {
		t.Fatalf("flag or image lost: %+v", out)
	}
	if !strings.Contains(out.Flatten(), "/large_tool_results/___a_b_c.txt") {
		t.Fatalf("id not sanitized: %.200s", out.Flatten())
	}
}

func TestOffloadHeadCapsHugeLine(t *testing.T) {
	out := wrapToolWith(t, Config{Backend: memory.New()}, "x", "c", tool.Text(strings.Repeat("y", 500_000)))
	if len(out.Flatten()) > 10_000 {
		t.Fatalf("single huge line defeated offload: %d bytes", len(out.Flatten()))
	}
}

// failingBackend rejects every write.
type failingBackend struct{ backend.Backend }

func (failingBackend) Write(context.Context, string, []byte) error { return errors.New("disk full") }

func TestOffloadWriteFailureTruncates(t *testing.T) {
	cfg := Config{Backend: failingBackend{memory.New()}, OffloadChars: 1000}
	out := wrapToolWith(t, cfg, "x", "c", tool.Text(bigOutput()))
	text := out.Flatten()
	if !strings.Contains(text, "[truncated") || len(text) > 1100 {
		t.Fatalf("want truncated text, got %d bytes: %.100s", len(text), text)
	}
}

func requestMessages(t *testing.T, cfg Config, msgs []llm.Message) []llm.Message {
	t.Helper()
	req := &agent.ModelRequest{Request: llm.Request{Messages: msgs}}
	var got []llm.Message
	_, err := New(cfg).(agent.ModelWrapper).WrapModel(context.Background(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
		got = r.Request.Messages
		return &llm.Response{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLargeUserMessageViewOnly(t *testing.T) {
	b := memory.New()
	huge := "intro\n" + strings.Repeat("z", MessageOffloadChars+10)
	history := []llm.Message{llm.UserText(huge), {Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock(huge)}}}

	first := requestMessages(t, Config{Backend: b}, history)
	if history[0].Content[0].Text != huge {
		t.Fatal("state message mutated")
	}
	text := first[0].Content[0].Text
	if !strings.HasPrefix(text, "intro\n") || !strings.Contains(text, "Message too large (") || len(text) > 3000 {
		t.Fatalf("user block not replaced: %.200s", text)
	}
	if first[1].Content[0].Text != huge {
		t.Fatal("assistant text must not be offloaded")
	}
	start := strings.Index(text, "/large_messages/")
	path := text[start : start+len("/large_messages/")+12+len(".txt")]
	if got := readAll(t, b, path); got != huge {
		t.Fatalf("stored message differs (len %d)", len(got))
	}

	second := requestMessages(t, Config{Backend: b}, history)
	if second[0].Content[0].Text != text {
		t.Fatal("second turn produced a different replacement")
	}
}

func TestLargeUserMessageWithoutBackendUnchanged(t *testing.T) {
	huge := strings.Repeat("z", MessageOffloadChars+10)
	got := requestMessages(t, Config{}, []llm.Message{llm.UserText(huge)})
	if got[0].Content[0].Text != huge {
		t.Fatal("block changed without a backend")
	}
}
