package agent

import (
	"context"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// chainModel wraps inner with every ModelWrapper in mws; mws[0] ends up
// outermost. Middleware that do not implement ModelWrapper are skipped. The
// chain is built once, so the returned handler is cheap to call per turn.
func chainModel(mws []Middleware, inner ModelHandler) ModelHandler {
	h := inner
	for i := len(mws) - 1; i >= 0; i-- {
		w, ok := mws[i].(ModelWrapper)
		if !ok {
			continue
		}
		h = wrapModelHandler(w, h)
	}
	return h
}

func wrapModelHandler(w ModelWrapper, next ModelHandler) ModelHandler {
	return func(ctx context.Context, req *ModelRequest) (*llm.Response, error) {
		return w.WrapModel(ctx, req, next)
	}
}

// chainTool wraps inner with every ToolWrapper in mws; mws[0] ends up
// outermost. Middleware that do not implement ToolWrapper are skipped.
func chainTool(mws []Middleware, inner ToolHandler) ToolHandler {
	h := inner
	for i := len(mws) - 1; i >= 0; i-- {
		w, ok := mws[i].(ToolWrapper)
		if !ok {
			continue
		}
		h = wrapToolHandler(w, h)
	}
	return h
}

func wrapToolHandler(w ToolWrapper, next ToolHandler) ToolHandler {
	return func(ctx context.Context, call *ToolCall) (tool.Result, error) {
		return w.WrapTool(ctx, call, next)
	}
}
