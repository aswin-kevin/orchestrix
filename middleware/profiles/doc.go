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
// (guard, hitl, hooks, planmode) cannot be excluded: agent.New fails for a
// model whose adjustment names it, so a subagent with such a model fails
// when it is built.
package profiles
