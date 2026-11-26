package agent

import (
	"context"
	"fmt"
	"slices"
)

// MiddlewareExcluder is implemented by middleware that removes other
// middleware for some models. New calls ExcludeMiddleware(Config.Model) on
// every excluder of the composed stack and removes the named middleware;
// names matching nothing are ignored and an excluder never removes itself.
// Naming a middleware that is Required makes New fail. A subagent built
// with another Model is filtered for its own model.
//
// Only the composed stack is filtered: Config.Middleware, as returned by
// Agent.Config, stays whole. Subagents and launchers build children from it
// and filter again for the child's model, so a middleware removed for a
// parent can still serve a child that runs another model. A nil or empty
// result removes nothing.
type MiddlewareExcluder interface {
	ExcludeMiddleware(model string) []string
}

// Required is implemented by middleware that a MiddlewareExcluder must not
// remove, such as approval and plan-mode middleware: removing them for one
// model would silently drop a safety check. New fails when an excluder
// names a middleware whose Required reports true. Exclusion the host asks
// for (Compose's exclude list) is not affected. Protection follows the
// implementation, not the name: a middleware that replaces hitl, hooks or
// planmode by name must implement Required itself to stay protected.
type Required interface{ Required() bool }

// filterExcluded returns stack without the middleware its excluders name for
// model. The input is not modified; without excluders it is returned as is.
// It fails when an excluder names a middleware of stack that is Required.
func filterExcluded(stack []Middleware, model string) ([]Middleware, error) {
	var drop []string
	for _, m := range stack {
		ex, ok := m.(MiddlewareExcluder)
		if !ok {
			continue
		}
		for _, name := range ex.ExcludeMiddleware(model) {
			if name == m.Name() {
				continue
			}
			if required(stack, name) {
				return nil, fmt.Errorf("agent: middleware %q cannot be excluded (named by %q)", name, m.Name())
			}
			drop = append(drop, name)
		}
	}
	if len(drop) == 0 {
		return stack, nil
	}
	out := make([]Middleware, 0, len(stack))
	for _, m := range stack {
		if !slices.Contains(drop, m.Name()) {
			out = append(out, m)
		}
	}
	return out, nil
}

// required reports whether a middleware of stack called name is Required.
func required(stack []Middleware, name string) bool {
	return slices.ContainsFunc(stack, func(m Middleware) bool {
		r, ok := m.(Required)
		return ok && m.Name() == name && r.Required()
	})
}

// ToolHider is implemented by middleware that hides tools from the model for
// the run in ctx, such as a per-model exclusion. Middleware that lists or
// searches tools (deferred's search_tools) consult ToolHidden and leave out
// what is hidden, so a hidden tool is not leaked through them. HidesTool must
// be fast (no I/O) and safe for concurrent use.
type ToolHider interface {
	HidesTool(ctx context.Context, name string) bool
}

// ToolHidden reports whether any middleware of the composed stack of the run
// ctx belongs to hides the tool name (see ToolHider). It is false outside a
// run.
func ToolHidden(ctx context.Context, name string) bool {
	info, ok := Info(ctx)
	if !ok || info.Agent == nil {
		return false
	}
	for _, m := range info.Agent.stack {
		if h, ok := m.(ToolHider); ok && h.HidesTool(ctx, name) {
			return true
		}
	}
	return false
}
