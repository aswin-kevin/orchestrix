package agent

import (
	"errors"
	"fmt"
	"slices"

	"github.com/aswin-kevin/orchestrix/llm"
)

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

func (r *run) afterAgent() error {
	for _, mw := range slices.Backward(r.agent.stack) {
		if h, ok := mw.(AfterAgent); ok {
			if err := h.AfterAgent(r.ctx, r.state); err != nil {
				return hookError(mw, "AfterAgent", err)
			}
		}
	}
	return nil
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
