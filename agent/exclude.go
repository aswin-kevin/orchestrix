package agent

import "slices"

// MiddlewareExcluder is implemented by middleware that removes other
// middleware for some models. New calls ExcludeMiddleware(Config.Model) on
// every excluder of the composed stack and removes the named middleware;
// names matching nothing are ignored and an excluder never removes itself.
// A subagent built with another Model is filtered for its own model.
//
// Only the composed stack is filtered: Config.Middleware, as returned by
// Agent.Config, stays whole. Subagents and launchers build children from it
// and filter again for the child's model, so a middleware removed for a
// parent can still serve a child that runs another model. A nil or empty
// result removes nothing.
type MiddlewareExcluder interface {
	ExcludeMiddleware(model string) []string
}

// filterExcluded returns stack without the middleware its excluders name for
// model. The input is not modified; without excluders it is returned as is.
func filterExcluded(stack []Middleware, model string) []Middleware {
	var drop []string
	for _, m := range stack {
		ex, ok := m.(MiddlewareExcluder)
		if !ok {
			continue
		}
		for _, name := range ex.ExcludeMiddleware(model) {
			if name != m.Name() {
				drop = append(drop, name)
			}
		}
	}
	if len(drop) == 0 {
		return stack
	}
	out := make([]Middleware, 0, len(stack))
	for _, m := range stack {
		if !slices.Contains(drop, m.Name()) {
			out = append(out, m)
		}
	}
	return out
}
