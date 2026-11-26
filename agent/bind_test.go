package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// binderStub records every agent it is bound to and returns err.
type binderStub struct {
	id    string
	err   error
	bound *[]*Agent
}

func (b binderStub) Name() string { return b.id }

func (b binderStub) Bind(a *Agent) error {
	*b.bound = append(*b.bound, a)
	return b.err
}

func TestBinderCalledOnce(t *testing.T) {
	var first, second []*Agent
	a := mustNew(t, Config{
		Provider:   llmtest.New(llmtest.Turn{Text: "x"}),
		Middleware: []Middleware{binderStub{id: "one", bound: &first}, binderStub{id: "two", bound: &second}},
	})
	if len(first) != 1 || first[0] != a || len(second) != 1 || second[0] != a {
		t.Fatalf("Bind calls = %v, %v; want one each with the built agent", first, second)
	}
	// A child built from the agent's configuration binds again, with
	// itself.
	child := mustNew(t, a.Config())
	if len(first) != 2 || first[1] != child {
		t.Fatalf("Bind calls after a child = %v, want the child second", first)
	}
}

func TestBinderErrorFailsNew(t *testing.T) {
	var bound []*Agent
	boom := errors.New("boom")
	_, err := New(Config{
		Provider:   llmtest.New(llmtest.Turn{Text: "x"}),
		Middleware: []Middleware{binderStub{id: "b", err: boom, bound: &bound}},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("New = %v, want the Bind error", err)
	}
}

func TestInputDepthMarksRunNested(t *testing.T) {
	log := newInfoLog()
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "x"}), Middleware: []Middleware{infoProbe{log}}})
	in := userInput("go")
	in.Depth = 2
	if _, final := collect(t, a.Run(context.Background(), in)); final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v", final)
	}
	if info := log.only(t, "BeforeAgent"); info.Depth != 2 || info.ParentToolID != "" {
		t.Fatalf("Info = %+v, want depth 2 with no parent call", info)
	}
}
