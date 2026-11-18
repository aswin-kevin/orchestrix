package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/aswin-kevin/orchestrix/llm"
)

// ErrStopRun ends a run on purpose. When a BeforeAgent, BeforeModel or
// AfterModel hook, or a ToolWrapper (or the tool itself), returns an error
// wrapping it, the run ends with ReasonHookStopped and Final.Err is that
// error. A tool call that stops the run is answered with an error result, and
// the calls of its turn that had not started are answered as skipped, so
// every tool_use keeps its tool_result. AfterAgent still runs.
var ErrStopRun = errors.New("agent: run stopped on purpose")

// Hook runners. Before* hooks run in stack order and After* hooks in reverse
// stack order (see the package documentation); each stops at the first error.

func (r *run) beforeAgent() error {
	for _, mw := range r.agent.stack {
		if h, ok := mw.(BeforeAgent); ok {
			if err := h.BeforeAgent(r.ctx, r.state); err != nil {
				return hookError(mw, "BeforeAgent", err)
			}
		}
	}
	return nil
}

func (r *run) beforeModel() error {
	for _, mw := range r.agent.stack {
		if h, ok := mw.(BeforeModel); ok {
			if err := h.BeforeModel(r.ctx, r.state); err != nil {
				return hookError(mw, "BeforeModel", err)
			}
		}
	}
	return nil
}

func (r *run) afterModel(resp *llm.Response) error {
	for _, mw := range slices.Backward(r.agent.stack) {
		if h, ok := mw.(AfterModel); ok {
			if err := h.AfterModel(r.ctx, r.state, resp); err != nil {
				return hookError(mw, "AfterModel", err)
			}
		}
	}
	return nil
}

// afterAgent runs the AfterAgent hooks under a context that keeps the run's
// values but not its cancellation: they must get to clean up and record the
// outcome even of a run that was cancelled.
func (r *run) afterAgent() error {
	ctx := context.WithoutCancel(r.ctx)
	for _, mw := range slices.Backward(r.agent.stack) {
		if h, ok := mw.(AfterAgent); ok {
			if err := h.AfterAgent(ctx, r.state); err != nil {
				return hookError(mw, "AfterAgent", err)
			}
		}
	}
	return nil
}

// hookFailure is the Final for a run a Before* or AfterModel hook ended:
// ReasonHookStopped when the hook asked to stop, ReasonMiddlewareError when
// it failed.
func (r *run) hookFailure(err error) *Final {
	if errors.Is(err, ErrStopRun) {
		return r.finish(ReasonHookStopped, err)
	}
	return r.finish(ReasonMiddlewareError, err)
}

// runAfterAgent runs the AfterAgent hooks and folds a failure into final. A
// run that had otherwise succeeded ends with ReasonMiddlewareError; a run
// that had already failed keeps its reason and gains the hook's error.
func (r *run) runAfterAgent(final *Final) {
	err := r.afterAgent()
	if err == nil {
		return
	}
	if final.Err == nil {
		final.Reason = ReasonMiddlewareError
	}
	final.Err = errors.Join(final.Err, err)
}

func hookError(mw Middleware, hook string, err error) error {
	return fmt.Errorf("agent: middleware %q %s: %w", mw.Name(), hook, err)
}
