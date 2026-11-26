package orchestrix_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/guard"
	"github.com/aswin-kevin/orchestrix/permission"
)

// recordingDecider records the text it is asked about and answers "no".
type recordingDecider struct{ seen []string }

func (d *recordingDecider) Decide(_ context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	d.seen = append(d.seen, req.Input[0].Text)
	answers := map[string]llm.Answer{}
	for _, q := range req.Questions {
		answers[q.Name] = llm.Answer{Type: llm.QuestionYesNo}
	}
	return &llm.DecisionResponse{Answers: answers}, nil
}

// The decider must see a tool call's arguments as the tool will receive
// them, not with JSON escapes that hide their meaning.
func TestGuardSeesDecodedToolArguments(t *testing.T) {
	d := &recordingDecider{}
	g, err := guard.New(guard.Config{Decider: d, Checks: []guard.Check{
		{Name: "deletes", Question: "Does this delete files?", On: guard.ToolCall},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// JSON escapes for "rm"
	raw := "{\"path\":\"/clean.sh\",\"content\":\"\\u0072\\u006d -rf /\"}"
	model := llmtest.New(
		llmtest.Turn{RawToolInput: true, ToolCalls: []llmtest.Call{{ID: "1", Name: "write_file", Input: raw}}},
		llmtest.Turn{Text: "done"},
	)
	a, err := orchestrix.NewWithProvider(model, orchestrix.WithMiddleware(g), orchestrix.WithPermissionMode(permission.ModeBypass))
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range a.Run(t.Context(), orchestrix.Text("go")) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(d.seen) != 1 || !strings.Contains(d.seen[0], "rm -rf /") {
		t.Fatalf("decider saw %q, want the decoded arguments containing %q", d.seen, "rm -rf /")
	}
}
