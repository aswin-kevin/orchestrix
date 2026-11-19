package askuser

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

type obj = map[string]any

func opts(labels ...string) []obj {
	out := make([]obj, len(labels))
	for i, l := range labels {
		out[i] = obj{"label": l}
	}
	return out
}

func dbQuestion() obj {
	return obj{"question": "Which database?", "header": "Database", "options": []obj{
		{"label": "Postgres", "description": "Server database"},
		{"label": "SQLite"},
	}}
}

func askCall(questions ...obj) llmtest.Call {
	return llmtest.Call{ID: "q", Name: ToolAskUser, Input: obj{"questions": append([]obj{}, questions...)}}
}

func newAgent(t *testing.T, cfg Config, ac agent.Config, turns ...llmtest.Turn) (*agent.Agent, *llmtest.Scripted) {
	t.Helper()
	p := llmtest.New(turns...)
	ac.Provider = p
	ac.Middleware = append(ac.Middleware, New(cfg))
	a, err := agent.New(ac)
	if err != nil {
		t.Fatal(err)
	}
	return a, p
}

func drain(t *testing.T, seq func(func(agent.Event, error) bool)) *agent.Final {
	t.Helper()
	var final *agent.Final
	for ev := range seq {
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatal("no final event")
	}
	return final
}

func run(t *testing.T, a *agent.Agent) *agent.Final {
	t.Helper()
	return drain(t, a.Run(context.Background(), agent.Input{Messages: []llm.Message{llm.UserText("go")}}))
}

// resultFor returns the text and error flag of the last tool result for id.
func resultFor(t *testing.T, p *llmtest.Scripted, id string) (string, bool) {
	t.Helper()
	var text string
	var isErr, found bool
	for _, req := range p.Requests {
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type != llm.BlockToolResult || b.ToolUseID != id {
					continue
				}
				found, isErr = true, b.IsError
				text = ""
				for _, c := range b.Content {
					text += c.Text
				}
			}
		}
	}
	if !found {
		t.Fatalf("no result for %q", id)
	}
	return text, isErr
}

// interrupted runs a call that must pause, returning the final and request.
func interrupted(t *testing.T, a *agent.Agent) (*agent.Final, agent.ActionRequest) {
	t.Helper()
	final := run(t, a)
	if final.Reason != agent.ReasonInterrupted {
		t.Fatalf("reason = %s (%v), want interrupted", final.Reason, final.Err)
	}
	intr, ok := agent.Pending(final.State)
	if !ok || len(intr.Requests) != 1 {
		t.Fatalf("pending = %+v", intr)
	}
	return final, intr.Requests[0]
}

func TestAskUserInterruptAndRespond(t *testing.T) {
	a, _ := newAgent(t, Config{}, agent.Config{}, llmtest.Turn{ToolCalls: []llmtest.Call{askCall(dbQuestion())}})
	final, req := interrupted(t, a)
	if req.Tool != ToolAskUser || !slices.Equal(req.Allowed, []agent.DecisionType{agent.Respond, agent.Reject}) {
		t.Fatalf("request = %+v", req)
	}
	qs, err := Questions(req)
	if err != nil || len(qs) != 1 || qs[0].Question != "Which database?" || qs[0].Options[0].Label != "Postgres" {
		t.Fatalf("Questions = %+v, %v", qs, err)
	}
	for _, want := range []string{"1. [Database] Which database?", "1) Postgres - Server database", "2) SQLite"} {
		if !strings.Contains(req.Reason, want) {
			t.Fatalf("reason %q lacks %q", req.Reason, want)
		}
	}

	// A structured answer.
	b, pb := newAgent(t, Config{}, agent.Config{}, llmtest.Turn{Text: "done"})
	drain(t, b.Resume(context.Background(), "", final.State.Clone(), []agent.Decision{
		Respond("q", []Answer{{Question: "Which database?", Selected: []string{"Postgres"}}}),
	}))
	if got, isErr := resultFor(t, pb, "q"); isErr || got != "User answers:\n- Which database?: Postgres" {
		t.Fatalf("result = %q (error %v)", got, isErr)
	}

	// Free text answers every question.
	c, pc := newAgent(t, Config{}, agent.Config{}, llmtest.Turn{Text: "done"})
	drain(t, c.Resume(context.Background(), "", final.State.Clone(), []agent.Decision{
		{ToolUseID: "q", Type: agent.Respond, Message: "use sqlite"},
	}))
	if got, _ := resultFor(t, pc, "q"); !strings.Contains(got, "use sqlite") {
		t.Fatalf("free-text result = %q", got)
	}
}

func TestAskUserCallback(t *testing.T) {
	var got []Question
	cfg := Config{Ask: func(_ context.Context, qs []Question) ([]Answer, error) {
		got = qs
		return []Answer{{Selected: []string{"SQLite", "Mongo"}, Other: "maybe both"}}, nil
	}}
	a, p := newAgent(t, cfg, agent.Config{},
		llmtest.Turn{ToolCalls: []llmtest.Call{askCall(dbQuestion())}},
		llmtest.Turn{Text: "done"})
	if f := run(t, a); f.Reason != agent.ReasonCompleted {
		t.Fatalf("reason = %s (%v)", f.Reason, f.Err)
	}
	if len(got) != 1 || got[0].Header != "Database" {
		t.Fatalf("callback questions = %+v", got)
	}
	want := "User answers:\n- Which database?: SQLite; other: Mongo, maybe both"
	if text, isErr := resultFor(t, p, "q"); isErr || text != want {
		t.Fatalf("result = %q (error %v), want %q", text, isErr, want)
	}
}

func TestAskUserCallbackError(t *testing.T) {
	cfg := Config{Ask: func(context.Context, []Question) ([]Answer, error) { return nil, errors.New("no terminal") }}
	a, p := newAgent(t, cfg, agent.Config{},
		llmtest.Turn{ToolCalls: []llmtest.Call{askCall(dbQuestion())}},
		llmtest.Turn{Text: "done"})
	run(t, a)
	if text, isErr := resultFor(t, p, "q"); !isErr || !strings.Contains(text, "no terminal") {
		t.Fatalf("result = %q (error %v)", text, isErr)
	}
}

func TestAskUserRejectDeclines(t *testing.T) {
	a, _ := newAgent(t, Config{}, agent.Config{}, llmtest.Turn{ToolCalls: []llmtest.Call{askCall(dbQuestion())}})
	final, _ := interrupted(t, a)
	b, p := newAgent(t, Config{}, agent.Config{}, llmtest.Turn{Text: "done"})
	drain(t, b.Resume(context.Background(), "", final.State, []agent.Decision{
		{ToolUseID: "q", Type: agent.Reject, Message: "not now"},
	}))
	if text, isErr := resultFor(t, p, "q"); isErr || text != "The user declined to answer. not now" {
		t.Fatalf("result = %q (error %v)", text, isErr)
	}
}

func TestAskUserValidation(t *testing.T) {
	q := func(text string, o []obj) obj { return obj{"question": text, "options": o} }
	long := func(n int) string { return strings.Repeat("x", n) }
	cases := []struct {
		name string
		qs   []obj
		want string
	}{
		{"no questions", []obj{}, "1-4 questions"},
		{"five questions", []obj{q("a", opts("x", "y")), q("b", opts("x", "y")), q("c", opts("x", "y")), q("d", opts("x", "y")), q("e", opts("x", "y"))}, "1-4 questions"},
		{"empty question", []obj{q(" ", opts("x", "y"))}, "no text"},
		{"one option", []obj{q("a", opts("x"))}, "2-4 options"},
		{"five options", []obj{q("a", opts("1", "2", "3", "4", "5"))}, "2-4 options"},
		{"empty label", []obj{q("a", opts("x", ""))}, "no label"},
		{"duplicate labels", []obj{q("a", opts("x", "x"))}, "repeats"},
		{"long header", []obj{{"question": "a", "header": long(MaxHeaderChars + 1), "options": opts("x", "y")}}, "header"},
		{"long question", []obj{q(long(MaxQuestionRunes+1), opts("x", "y"))}, "longer than 1000"},
		{"long label", []obj{q("a", opts("x", long(MaxLabelRunes+1)))}, "longer than 200"},
		{"long description", []obj{q("a", []obj{{"label": "x", "description": long(MaxDescriptionRunes + 1)}, {"label": "y"}})}, "longer than 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Ask: func(context.Context, []Question) ([]Answer, error) {
				t.Error("callback must not run for invalid questions")
				return nil, nil
			}}
			a, p := newAgent(t, cfg, agent.Config{},
				llmtest.Turn{ToolCalls: []llmtest.Call{askCall(tc.qs...)}},
				llmtest.Turn{Text: "done"})
			run(t, a)
			if text, isErr := resultFor(t, p, "q"); !isErr || !strings.Contains(text, tc.want) {
				t.Fatalf("result = %q (error %v), want error containing %q", text, isErr, tc.want)
			}
		})
	}
}

func TestAskUserHeaderAtLimitAccepted(t *testing.T) {
	cfg := Config{Ask: func(context.Context, []Question) ([]Answer, error) { return []Answer{{Selected: []string{"x"}}}, nil }}
	q := obj{"question": "a", "header": strings.Repeat("é", MaxHeaderChars), "options": opts("x", "y")}
	a, p := newAgent(t, cfg, agent.Config{}, llmtest.Turn{ToolCalls: []llmtest.Call{askCall(q)}}, llmtest.Turn{Text: "done"})
	run(t, a)
	if _, isErr := resultFor(t, p, "q"); isErr {
		t.Fatal("a header of exactly MaxHeaderChars runes must be accepted")
	}
}

func TestAskUserAllowedInPlanMode(t *testing.T) {
	cfg := Config{Ask: func(context.Context, []Question) ([]Answer, error) {
		return []Answer{{Selected: []string{"Postgres"}}}, nil
	}}
	a, p := newAgent(t, cfg, agent.Config{PermissionMode: permission.ModePlan},
		llmtest.Turn{ToolCalls: []llmtest.Call{askCall(dbQuestion())}},
		llmtest.Turn{Text: "done"})
	run(t, a)
	if text, isErr := resultFor(t, p, "q"); isErr || !strings.Contains(text, "Postgres") {
		t.Fatalf("result = %q (error %v)", text, isErr)
	}
}

func TestAskUserNoApprovalNeeded(t *testing.T) {
	deny := func(context.Context, permission.Request, string) permission.Decision {
		t.Error("ask_user must not reach the approver")
		return permission.Denied("no")
	}
	cfg := Config{Ask: func(context.Context, []Question) ([]Answer, error) {
		return []Answer{{Selected: []string{"Postgres"}}}, nil
	}}
	a, p := newAgent(t, cfg, agent.Config{Approver: deny},
		llmtest.Turn{ToolCalls: []llmtest.Call{askCall(dbQuestion())}},
		llmtest.Turn{Text: "done"})
	run(t, a)
	if _, isErr := resultFor(t, p, "q"); isErr {
		t.Fatal("ask_user was refused")
	}
}

func TestAskUserStripsControlCharacters(t *testing.T) {
	q := obj{
		"question": "Drop\x1b[2J it?\nsecond line\x07",
		"header":   "H\x1b",
		"options": []obj{
			{"label": "Yes\x1b]0;pwned\x07", "description": "de\x00sc\r"},
			{"label": "No"},
		},
	}
	a, _ := newAgent(t, Config{}, agent.Config{}, llmtest.Turn{ToolCalls: []llmtest.Call{askCall(q)}})
	_, req := interrupted(t, a)
	for _, r := range req.Reason {
		if r != '\n' && r < 0x20 || r == 0x7f {
			t.Fatalf("reason holds control character %q: %q", r, req.Reason)
		}
	}
	if !strings.Contains(req.Reason, "Drop[2J it?\nsecond line") {
		t.Fatalf("reason lost its newline or text: %q", req.Reason)
	}
	qs, err := Questions(req)
	if err != nil || strings.ContainsAny(qs[0].Question+qs[0].Options[0].Label+qs[0].Options[0].Description, "\x1b\x07\x00\r") {
		t.Fatalf("Questions kept control characters: %+v, %v", qs, err)
	}
}

func TestAskUserCapsFreeTextAnswer(t *testing.T) {
	a, _ := newAgent(t, Config{}, agent.Config{}, llmtest.Turn{ToolCalls: []llmtest.Call{askCall(dbQuestion())}})
	final, _ := interrupted(t, a)
	b, p := newAgent(t, Config{}, agent.Config{}, llmtest.Turn{Text: "done"})
	drain(t, b.Resume(context.Background(), "", final.State, []agent.Decision{
		{ToolUseID: "q", Type: agent.Respond, Message: strings.Repeat("é", MaxAnswerRunes+500)},
	}))
	text, _ := resultFor(t, p, "q")
	if got := strings.Count(text, "é"); got != MaxAnswerRunes {
		t.Fatalf("answer kept %d runes, want %d", got, MaxAnswerRunes)
	}
}

// runInSubagent runs a child agent (Depth 1) that calls ask_user, from a
// parent's tool, and returns what the child's model saw.
func runInSubagent(t *testing.T, cfg Config) (string, bool) {
	t.Helper()
	child, childP := newAgent(t, cfg, agent.Config{},
		llmtest.Turn{ToolCalls: []llmtest.Call{askCall(dbQuestion())}},
		llmtest.Turn{Text: "child done"})
	spawn := tool.New("spawn", "Runs a child.", func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
		var last *agent.Final
		for ev := range child.Run(ctx, agent.Input{Messages: []llm.Message{llm.UserText("go")}}) {
			if ev.Kind == agent.KindFinal {
				last = ev.Final
			}
		}
		return tool.Text(string(last.Reason)), nil
	}, tool.WithReadOnly())
	parent, parentP := newAgent(t, Config{}, agent.Config{Tools: []tool.Tool{spawn}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s", Name: "spawn", Input: obj{}}}},
		llmtest.Turn{Text: "done"})
	run(t, parent)
	if reason, _ := resultFor(t, parentP, "s"); reason != string(agent.ReasonCompleted) {
		t.Fatalf("child ended with %q, want completed (no interrupt)", reason)
	}
	return resultFor(t, childP, "q")
}

func TestAskUserRefusedInSubagentWithoutAsk(t *testing.T) {
	text, isErr := runInSubagent(t, Config{})
	if !isErr || !strings.Contains(text, "subagent") || !strings.Contains(text, "Config.Ask") {
		t.Fatalf("result = %q (error %v)", text, isErr)
	}
}

func TestAskUserInSubagentUsesAsk(t *testing.T) {
	text, isErr := runInSubagent(t, Config{Ask: func(context.Context, []Question) ([]Answer, error) {
		return []Answer{{Selected: []string{"SQLite"}}}, nil
	}})
	if isErr || !strings.Contains(text, "SQLite") {
		t.Fatalf("result = %q (error %v)", text, isErr)
	}
}

func TestPromptSegment(t *testing.T) {
	system := func(cfg Config) []string {
		a, p := newAgent(t, cfg, agent.Config{System: []string{"host"}}, llmtest.Turn{Text: "hi"})
		run(t, a)
		return p.Requests[0].System
	}
	if got := system(Config{}); len(got) != 2 || got[0] != "host" || got[1] != DefaultPrompt {
		t.Fatalf("default system = %q", got)
	}
	off := ""
	if got := system(Config{Prompt: &off}); len(got) != 1 {
		t.Fatalf("disabled system = %q", got)
	}
}
