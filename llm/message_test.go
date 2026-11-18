package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMessageTextAndToolUses(t *testing.T) {
	m := Message{Role: RoleAssistant, Content: []Block{
		TextBlock("a"),
		{Type: BlockToolUse, ID: "t1", Name: "read_file"},
		TextBlock("b"),
	}}
	if got := m.Text(); got != "ab" {
		t.Errorf("Text() = %q, want %q", got, "ab")
	}
	uses := m.ToolUses()
	if len(uses) != 1 || uses[0].ID != "t1" {
		t.Errorf("ToolUses() = %+v, want one use with ID t1", uses)
	}
}

func TestUsageMergeEcho(t *testing.T) {
	u := Usage{Input: 10}
	u.MergeEcho(Usage{Input: 10, Output: 5})
	u.MergeEcho(Usage{Output: 7})
	want := Usage{Input: 10, Output: 12}
	if u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestUsageAdd(t *testing.T) {
	u := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}
	u.Add(Usage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40})
	want := Usage{Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44}
	if u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestToolResultText(t *testing.T) {
	b := ToolResultText("t1", "boom", true)
	if b.Type != BlockToolResult || b.ToolUseID != "t1" || !b.IsError {
		t.Errorf("unexpected block %+v", b)
	}
	if len(b.Content) != 1 || b.Content[0].Text != "boom" {
		t.Errorf("content = %+v, want one text block %q", b.Content, "boom")
	}
}

func TestMessageJSONRoundTrip(t *testing.T) {
	in := Message{
		Role: RoleAssistant,
		Content: []Block{
			{Type: BlockToolUse, ID: "t1", Name: "x", Input: json.RawMessage(`{"a":1}`),
				ProviderData: map[string]string{"k": "v"}},
		},
		Meta: map[string]string{"origin": "test"},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Message
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}

	plain, err := json.Marshal(UserText("hi"))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"role":"user","content":[{"type":"text","text":"hi"}]}`
	if string(plain) != want {
		t.Errorf("zero fields not omitted: %s", plain)
	}
}
