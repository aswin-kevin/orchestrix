package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// requiredStub is a middleware reporting req from Required.
type requiredStub struct {
	id  string
	req bool
}

func (r requiredStub) Name() string   { return r.id }
func (r requiredStub) Required() bool { return r.req }

func TestExcluderCannotRemoveRequired(t *testing.T) {
	_, err := New(Config{
		Provider:   llmtest.New(llmtest.Turn{Text: "x"}),
		Model:      "small-1",
		Middleware: []Middleware{excluder{name: "ex", remove: []string{"req"}}, requiredStub{id: "req", req: true}},
	})
	if err == nil || !strings.Contains(err.Error(), `"req"`) || !strings.Contains(err.Error(), `"ex"`) {
		t.Fatalf("err = %v, want one naming req and ex", err)
	}
}

func TestExcluderCannotRemoveRequiredForChildModel(t *testing.T) {
	ex := excluder{name: "ex", only: "small-1", remove: []string{"req"}}
	parent := newExcluded(t, "big-1", ex, requiredStub{id: "req", req: true})
	cfg := parent.Config()
	cfg.Model = "small-1"
	if _, err := New(cfg); err == nil {
		t.Fatal("child for the excluded model built, want an error")
	}
}

func TestExcluderRemovesNotRequired(t *testing.T) {
	a := newExcluded(t, "small-1", excluder{name: "ex", remove: []string{"req"}}, requiredStub{id: "req"})
	if got := names(a.stack); !reflect.DeepEqual(got, []string{"ex"}) {
		t.Fatalf("stack = %v, want the excluder alone", got)
	}
}

func TestComposeExcludeIgnoresRequired(t *testing.T) {
	base := []Middleware{requiredStub{id: "req", req: true}, ownerStub{id: "other"}}
	got, err := Compose(base, nil, []string{"req"})
	if err != nil {
		t.Fatal(err)
	}
	if n := names(got); !reflect.DeepEqual(n, []string{"other"}) {
		t.Fatalf("stack = %v, want req removed", n)
	}
}
