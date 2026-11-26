package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type greetIn struct {
	Name string `json:"name" desc:"who to greet"`
	Loud *bool  `json:"loud"`
}

func TestNewTypedDecodesAndValidates(t *testing.T) {
	calls := 0
	tl := New("greet", "greets", func(_ context.Context, in greetIn, _ *Call) (Result, error) {
		calls++
		return Text("hello " + in.Name), nil
	}, WithReadOnly(), WithConcurrent())

	res, err := tl.Call(t.Context(), json.RawMessage(`{"name":"bob"}`), &Call{})
	if err != nil || res.IsError || res.Flatten() != "hello bob" {
		t.Fatalf("valid call = %+v, %v", res, err)
	}

	for _, bad := range []string{`{}`, `{"name":5}`, `{"name":"x","nope":1}`, `not json`} {
		res, err := tl.Call(t.Context(), json.RawMessage(bad), &Call{})
		if err != nil {
			t.Errorf("input %s: Go error %v, want model-facing result", bad, err)
		}
		if !res.IsError || !strings.Contains(res.Flatten(), "invalid input") {
			t.Errorf("input %s: result = %+v, want invalid input error", bad, res)
		}
	}
	if calls != 1 {
		t.Errorf("fn ran %d times, want 1 (invalid input must not reach it)", calls)
	}

	in := json.RawMessage(`{}`)
	r, _ := As[ReadOnlyer](tl)
	c, _ := As[ConcurrencySafer](tl)
	if !r.ReadOnly(in) || !c.ConcurrencySafe(in) {
		t.Error("options not applied")
	}
}

func TestNewTypedTreatsEmptyInputAsEmptyObject(t *testing.T) {
	type none struct{}
	tl := New("ping", "pings", func(context.Context, none, *Call) (Result, error) { return Text("pong"), nil })
	res, err := tl.Call(t.Context(), nil, &Call{})
	if err != nil || res.IsError {
		t.Errorf("empty input = %+v, %v", res, err)
	}
}

func TestNewTypedPropagatesGoError(t *testing.T) {
	boom := errors.New("boom")
	tl := New("fail", "fails", func(context.Context, greetIn, *Call) (Result, error) { return Result{}, boom })
	if _, err := tl.Call(t.Context(), json.RawMessage(`{"name":"x"}`), &Call{}); !errors.Is(err, boom) {
		t.Errorf("error = %v, want boom", err)
	}
}

func TestNewTypedOptionsAndSchema(t *testing.T) {
	tl := New("w", "writes", func(context.Context, greetIn, *Call) (Result, error) { return Text(""), nil }, WithEdit(),
		WithSubject(func(json.RawMessage) string { return "s" }))
	if !tl.(Editor).IsEdit(nil) || tl.(Subjecter).Subject(nil) != "s" {
		t.Error("WithEdit/WithSubject not applied")
	}
	props := tl.Schema()["properties"].(map[string]any)
	if props["name"].(map[string]any)["description"] != "who to greet" {
		t.Errorf("schema = %v", tl.Schema())
	}
}

type countIn struct {
	Count int     `json:"count"`
	Limit int     `json:"limit,omitempty"`
	Label *string `json:"label"`
	Tags  []int   `json:"tags,omitempty"`
}

func TestNewTypedToleratesNullOptionalsAndIntegralNumbers(t *testing.T) {
	var got countIn
	tl := New("count", "counts", func(_ context.Context, in countIn, _ *Call) (Result, error) {
		got = in
		return Text("ok"), nil
	})
	res, err := tl.Call(t.Context(), json.RawMessage(`{"count":1e2,"limit":null,"label":null,"tags":[2.0,3]}`), &Call{})
	if err != nil || res.IsError {
		t.Fatalf("call = %+v, %v; want success", res, err)
	}
	if got.Count != 100 || got.Limit != 0 || got.Label != nil || len(got.Tags) != 2 || got.Tags[0] != 2 {
		t.Fatalf("decoded = %+v, want count 100, limit 0, nil label, tags [2 3]", got)
	}
	res, _ = tl.Call(t.Context(), json.RawMessage(`{"count":1.5}`), &Call{})
	if !res.IsError {
		t.Fatal("a fractional count was accepted")
	}
}
