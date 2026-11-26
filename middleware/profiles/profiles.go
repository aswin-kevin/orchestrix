// Package profiles adjusts an agent for the model it runs: a prompt suffix,
// replacement tool descriptions and tools or middleware left out. Weaker or
// differently trained models often need a firmer instruction, a shorter tool
// description or one less tool than a strong model does.
//
// # Patterns
//
// Models and ExcludeTools hold name patterns as in tool.MatchName, not path
// globs: * matches any run of characters, including "/" (so "gpt-*" does not
// match "org/gpt-5" but "*gpt-*" does), ? matches exactly one character, and
// nothing is cleaned or collapsed. A pattern must not be empty, blank or
// contain a NUL byte (tool.ValidPattern).
//
// # Which model
//
// The profile of a run is chosen by the model its agent is configured with
// (agent.Config.Model), for the model call, the tool call and the hiding of
// tools alike. A Request.Model rewritten by another middleware does not
// change it. Outside a run (no agent.Info in the context) the middleware
// does nothing.
//
// The unit is an Adjustment, selected by name patterns on the model name. It
// is not an llm.Profile, which describes what a model can do; an Adjustment
// describes how this agent treats it.
//
// # Excluded middleware
//
// ExcludeMiddleware is applied when the agent is built (see
// agent.MiddlewareExcluder), for Config.Model. Subagents with another model
// are filtered for their own model. A middleware that is agent.Required
// (hitl, hooks, planmode) cannot be excluded: agent.New fails for a model
// whose adjustment names it, so a subagent with such a model fails when it
// is built.
package profiles

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Name is the name of the profiles middleware.
const Name = "profiles"

// ExcludedText is the refusal given for a call to an excluded tool; its
// arguments are the tool name and the model name.
const ExcludedText = "tool %s is not available for model %s"

// Adjustment is a set of changes applied to the models it matches.
type Adjustment struct {
	// Models are name patterns (* and ?, see Patterns) on the model name;
	// the adjustment applies when any matches. At least one is required.
	Models []string
	// PromptSuffix is appended as the last system segment; "" adds none. It
	// is last among the segments added by middleware that wrap outside
	// profiles; the preset lists profiles last so that it is last overall.
	PromptSuffix string
	// ToolDescriptions maps a tool name to the description shown to the
	// model instead of the tool's own in its requests; nil or empty changes
	// none. Search results of other middleware (search_tools) still show the
	// tool's own description.
	ToolDescriptions map[string]string
	// ExcludeTools are name patterns on tool names: a matching tool is hidden
	// from the model and refused when called, and left out of search_tools.
	// A model that names a hidden tool anyway may be asked for approval
	// before the refusal. Nil excludes none.
	ExcludeTools []string
	// ExcludeMiddleware names middleware removed when the agent is built;
	// names matching nothing are ignored. "profiles" is rejected by New.
	// Naming a middleware that is agent.Required (hitl, hooks, planmode)
	// makes agent.New fail for a matching model: removing it would silently
	// drop its approvals, hooks or plan mode. No dependency check is made:
	// excluding a middleware that others rely on (filesystem, for instance)
	// leaves them configured and the agent still runs. Nil removes none.
	ExcludeMiddleware []string
}

// Config configures the profiles middleware.
type Config struct {
	// Adjustments are tried in order; every one whose Models match applies.
	// Suffixes append in order, a later description override wins and the
	// exclusions are unioned. Nil changes nothing.
	Adjustments []Adjustment
}

// Middleware is the profiles middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type Middleware struct {
	adjustments []Adjustment
}

// New validates cfg and returns the middleware. It fails for an adjustment
// without Models, a pattern that is empty, blank or holds a NUL byte, or an
// ExcludeMiddleware naming the profiles middleware itself. Excluding a
// middleware that is agent.Required is refused by agent.New instead, for
// the models the adjustment matches.
func New(cfg Config) (*Middleware, error) {
	adjs := make([]Adjustment, len(cfg.Adjustments))
	for i, a := range cfg.Adjustments {
		if len(a.Models) == 0 {
			return nil, fmt.Errorf("profiles: adjustment %d has no Models", i)
		}
		for _, p := range slices.Concat(a.Models, a.ExcludeTools) {
			if err := tool.ValidPattern(p); err != nil {
				return nil, fmt.Errorf("profiles: adjustment %d has an invalid pattern %q: %w", i, p, err)
			}
		}
		if slices.Contains(a.ExcludeMiddleware, Name) {
			return nil, errors.New("profiles: ExcludeMiddleware must not name profiles")
		}
		a.Models = slices.Clone(a.Models)
		a.ExcludeTools = slices.Clone(a.ExcludeTools)
		a.ExcludeMiddleware = slices.Clone(a.ExcludeMiddleware)
		a.ToolDescriptions = maps.Clone(a.ToolDescriptions)
		adjs[i] = a
	}
	return &Middleware{adjustments: adjs}, nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Match returns copies of the adjustments whose Models match model, in order.
func (m *Middleware) Match(model string) []Adjustment {
	out := m.matching(model)
	for i, a := range out {
		out[i].Models = slices.Clone(a.Models)
		out[i].ExcludeTools = slices.Clone(a.ExcludeTools)
		out[i].ExcludeMiddleware = slices.Clone(a.ExcludeMiddleware)
		out[i].ToolDescriptions = maps.Clone(a.ToolDescriptions)
	}
	return out
}

// matching returns the adjustments whose Models match model, in order,
// sharing their slices and maps with m: callers only read them. It spares
// the per-call cloning on hot paths (HidesTool runs per tool per search).
func (m *Middleware) matching(model string) []Adjustment {
	var out []Adjustment
	for _, a := range m.adjustments {
		if slices.ContainsFunc(a.Models, func(p string) bool { return tool.MatchName(p, model) }) {
			out = append(out, a)
		}
	}
	return out
}

// ExcludeMiddleware implements agent.MiddlewareExcluder.
func (m *Middleware) ExcludeMiddleware(model string) []string {
	var out []string
	for _, a := range m.matching(model) {
		out = append(out, a.ExcludeMiddleware...)
	}
	return out
}

// excluded reports whether an adjustment hides the tool name.
func excluded(adjs []Adjustment, name string) bool {
	for _, a := range adjs {
		if slices.ContainsFunc(a.ExcludeTools, func(p string) bool { return tool.MatchName(p, name) }) {
			return true
		}
	}
	return false
}

// forRun returns the adjustments for the model the run in ctx is configured
// with, and that model; ok is false outside a run.
func (m *Middleware) forRun(ctx context.Context) (adjs []Adjustment, model string, ok bool) {
	info, ok := agent.Info(ctx)
	if !ok {
		return nil, "", false
	}
	model = info.Model()
	return m.matching(model), model, true
}

// HidesTool implements agent.ToolHider.
func (m *Middleware) HidesTool(ctx context.Context, name string) bool {
	adjs, _, ok := m.forRun(ctx)
	return ok && excluded(adjs, name)
}

// WrapModel implements agent.ModelWrapper. It drops the excluded tools from
// the request, rewrites descriptions and appends the suffixes.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	adjs, _, ok := m.forRun(ctx)
	if !ok || len(adjs) == 0 {
		return next(ctx, req)
	}
	descriptions := map[string]string{}
	for _, a := range adjs {
		maps.Copy(descriptions, a.ToolDescriptions)
		if a.PromptSuffix != "" {
			req.Request.System = append(slices.Clone(req.Request.System), a.PromptSuffix)
		}
	}
	shown := make([]llm.ToolSchema, 0, len(req.Request.Tools))
	for _, s := range req.Request.Tools {
		if excluded(adjs, s.Name) {
			continue
		}
		if d, ok := descriptions[s.Name]; ok {
			s.Description = d
		}
		shown = append(shown, s)
	}
	req.Request.Tools = shown
	return next(ctx, req)
}

// WrapTool implements agent.ToolWrapper. It refuses a call to an excluded
// tool, whether the model called it directly or through a dispatcher.
func (m *Middleware) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	adjs, model, ok := m.forRun(ctx)
	if ok && excluded(adjs, call.Name) {
		return tool.Errorf(ExcludedText, call.Name, model), nil
	}
	return next(ctx, call)
}
