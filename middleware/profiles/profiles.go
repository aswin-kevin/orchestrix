// Package profiles adjusts an agent for the model it runs: a prompt suffix,
// replacement tool descriptions and tools or middleware left out. Weaker or
// differently trained models often need a firmer instruction, a shorter tool
// description or one less tool than a strong model does.
//
// The unit is an Adjustment, selected by glob patterns on the model name. It
// is not an llm.Profile, which describes what a model can do; an Adjustment
// describes how this agent treats it.
//
// # Excluded middleware
//
// ExcludeMiddleware is applied when the agent is built (see
// agent.MiddlewareExcluder), for Config.Model. Subagents with another model
// are filtered for their own model.
package profiles

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/glob"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// MiddlewareName is the name of the profiles middleware.
const MiddlewareName = "profiles"

// ExcludedText is the refusal given for a call to an excluded tool; its
// arguments are the tool name and the model name.
const ExcludedText = "tool %s is not available for model %s"

// Adjustment is a set of changes applied to the models it matches.
type Adjustment struct {
	// Models are glob patterns (* and ?) on the model name; the adjustment
	// applies when any matches. At least one is required.
	Models []string
	// PromptSuffix is appended as the last system segment; "" adds none.
	PromptSuffix string
	// ToolDescriptions maps a tool name to the description shown to the
	// model instead of the tool's own; nil or empty changes none.
	ToolDescriptions map[string]string
	// ExcludeTools are glob patterns on tool names: a matching tool is hidden
	// from the model and refused when called. Nil excludes none.
	ExcludeTools []string
	// ExcludeMiddleware names middleware removed when the agent is built;
	// names matching nothing are ignored, and "profiles" is rejected. Nil
	// removes none.
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
// ExcludeMiddleware naming the profiles middleware itself.
func New(cfg Config) (*Middleware, error) {
	adjs := make([]Adjustment, len(cfg.Adjustments))
	for i, a := range cfg.Adjustments {
		if len(a.Models) == 0 {
			return nil, fmt.Errorf("profiles: adjustment %d has no Models", i)
		}
		for _, p := range slices.Concat(a.Models, a.ExcludeTools) {
			if strings.TrimSpace(p) == "" || strings.ContainsRune(p, 0) {
				return nil, fmt.Errorf("profiles: adjustment %d has an invalid pattern %q", i, p)
			}
		}
		if slices.Contains(a.ExcludeMiddleware, MiddlewareName) {
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
func (m *Middleware) Name() string { return MiddlewareName }

// Match returns the adjustments whose Models match model, in order.
func (m *Middleware) Match(model string) []Adjustment {
	var out []Adjustment
	for _, a := range m.adjustments {
		if slices.ContainsFunc(a.Models, func(p string) bool { return glob.Match(p, model) }) {
			out = append(out, a)
		}
	}
	return out
}

// ExcludeMiddleware implements agent.MiddlewareExcluder.
func (m *Middleware) ExcludeMiddleware(model string) []string {
	var out []string
	for _, a := range m.Match(model) {
		out = append(out, a.ExcludeMiddleware...)
	}
	return out
}

// excluded reports whether a matching adjustment hides the tool name.
func excluded(adjs []Adjustment, name string) bool {
	for _, a := range adjs {
		if slices.ContainsFunc(a.ExcludeTools, func(p string) bool { return glob.Match(p, name) }) {
			return true
		}
	}
	return false
}

// runModel is the model of the run in ctx, or "".
func runModel(ctx context.Context) string {
	if info, ok := agent.Info(ctx); ok {
		return info.Agent.Config().Model
	}
	return ""
}

// WrapModel implements agent.ModelWrapper. It drops the excluded tools from
// the request, rewrites descriptions and appends the suffixes.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	model := req.Request.Model
	if model == "" {
		model = runModel(ctx)
	}
	adjs := m.Match(model)
	if len(adjs) == 0 {
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
	model := runModel(ctx)
	if excluded(m.Match(model), call.Name) {
		return tool.Errorf(ExcludedText, call.Name, model), nil
	}
	return next(ctx, call)
}
