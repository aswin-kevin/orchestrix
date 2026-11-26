package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// recorder is a middleware that logs entry and exit of both wrappers into a
// shared trace, optionally short-circuiting the model call.
type recorder struct {
	name         string
	trace        *[]string
	shortCircuit bool
}

func (r recorder) Name() string { return r.name }

func (r recorder) WrapModel(ctx context.Context, req *ModelRequest, next ModelHandler) (*llm.Response, error) {
	*r.trace = append(*r.trace, r.name)
	if r.shortCircuit {
		return &llm.Response{}, nil
	}
	resp, err := next(ctx, req)
	*r.trace = append(*r.trace, r.name)
	return resp, err
}

// plain implements no wrapper interface and must be ignored by the chains.
type plain struct{}

func (plain) Name() string { return "plain" }

func TestChainModelOrder(t *testing.T) {
	var trace []string
	mws := []Middleware{
		recorder{name: "a", trace: &trace},
		plain{},
		recorder{name: "b", trace: &trace},
		recorder{name: "c", trace: &trace},
	}
	h := chainModel(mws, func(context.Context, *ModelRequest) (*llm.Response, error) {
		trace = append(trace, "inner")
		return &llm.Response{}, nil
	})

	if _, err := h(t.Context(), &ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c", "inner", "c", "b", "a"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestChainModelShortCircuit(t *testing.T) {
	var trace []string
	mws := []Middleware{
		recorder{name: "a", trace: &trace},
		recorder{name: "b", trace: &trace, shortCircuit: true},
		recorder{name: "c", trace: &trace},
	}
	h := chainModel(mws, func(context.Context, *ModelRequest) (*llm.Response, error) {
		t.Fatal("inner must not run when a wrapper short-circuits")
		return nil, nil
	})

	if _, err := h(t.Context(), &ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "a"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestChainModelWithoutWrappersCallsInner(t *testing.T) {
	called := false
	h := chainModel([]Middleware{plain{}}, func(context.Context, *ModelRequest) (*llm.Response, error) {
		called = true
		return &llm.Response{}, nil
	})
	if _, err := h(t.Context(), &ModelRequest{}); err != nil || !called {
		t.Fatalf("called=%v err=%v", called, err)
	}
}

// inputRewriter replaces the tool input before delegating.
type inputRewriter struct{ input string }

func (inputRewriter) Name() string { return "rewriter" }

func (r inputRewriter) WrapTool(ctx context.Context, call *ToolCall, next ToolHandler) (tool.Result, error) {
	call.Input = json.RawMessage(r.input)
	return next(ctx, call)
}

func TestChainToolRewriteInput(t *testing.T) {
	var seen json.RawMessage
	h := chainTool([]Middleware{plain{}, inputRewriter{input: `{"x":2}`}}, func(_ context.Context, call *ToolCall) (tool.Result, error) {
		seen = call.Input
		return tool.Text("ok"), nil
	})

	res, err := h(t.Context(), &ToolCall{Input: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(seen) != `{"x":2}` {
		t.Fatalf("inner saw input %s, want {\"x\":2}", seen)
	}
	if got := res.Flatten(); got != "ok" {
		t.Fatalf("result = %q", got)
	}
}

// toolRecorder logs entry and exit of WrapTool into a shared trace.
type toolRecorder struct {
	name         string
	trace        *[]string
	shortCircuit bool
}

func (r toolRecorder) Name() string { return r.name }

func (r toolRecorder) WrapTool(ctx context.Context, call *ToolCall, next ToolHandler) (tool.Result, error) {
	*r.trace = append(*r.trace, r.name)
	if r.shortCircuit {
		return tool.Text("short"), nil
	}
	res, err := next(ctx, call)
	*r.trace = append(*r.trace, r.name)
	return res, err
}

func TestChainToolOrder(t *testing.T) {
	var trace []string
	mws := []Middleware{
		toolRecorder{name: "a", trace: &trace},
		plain{},
		toolRecorder{name: "b", trace: &trace},
	}
	h := chainTool(mws, func(context.Context, *ToolCall) (tool.Result, error) {
		trace = append(trace, "inner")
		return tool.Text("ok"), nil
	})
	if _, err := h(t.Context(), &ToolCall{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "inner", "b", "a"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}

func TestChainToolShortCircuit(t *testing.T) {
	var trace []string
	mws := []Middleware{
		toolRecorder{name: "a", trace: &trace},
		toolRecorder{name: "b", trace: &trace, shortCircuit: true},
		toolRecorder{name: "c", trace: &trace},
	}
	h := chainTool(mws, func(context.Context, *ToolCall) (tool.Result, error) {
		t.Fatal("inner must not run when a wrapper short-circuits")
		return tool.Result{}, nil
	})
	res, err := h(t.Context(), &ToolCall{})
	if err != nil || res.Flatten() != "short" {
		t.Fatalf("res=%q err=%v", res.Flatten(), err)
	}
	want := []string{"a", "b", "a"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}
