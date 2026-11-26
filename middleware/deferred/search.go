package deferred

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/tool"
)

// selectPrefix starts a search_tools query that names tools exactly.
const selectPrefix = "select:"

// noMatchText is the search_tools result when nothing matches.
const noMatchText = "No matching tools."

// SearchInput is the input of search_tools.
type SearchInput struct {
	// Query holds keywords matched against tool names and descriptions, or
	// select:name1,name2 for exact names.
	Query string `json:"query" desc:"Keywords matched against tool names and descriptions, or select:name1,name2 for exact names."`
	// Regex treats Query as a case-insensitive regular expression.
	Regex bool `json:"regex,omitempty" desc:"Treat query as a case-insensitive regular expression."`
	// MaxResults is the maximum number of tools to return.
	MaxResults int `json:"max_results,omitempty" desc:"Maximum tools to return."`
}

// CallInput is the input of call_tool. Input is optional: a missing or null
// input calls the tool with {}.
type CallInput struct {
	// Name is the exact name of the tool to call.
	Name string `json:"name" desc:"Exact name of the tool to call."`
	// Input is the tool's input object; nil or null calls it with {}.
	Input json.RawMessage `json:"input,omitempty" desc:"The tool's input object."`
}

// searchTool returns search_tools. It only reads the tool set and records
// what it found in the run's own unlock set, so it is read-only.
func (m *Middleware) searchTool() tool.Tool {
	return tool.New(ToolSearch,
		"Search the tools whose schemas are not loaded yet. Matching tools are listed with their input schemas and can be called directly from your next turn on.",
		m.runSearch,
		tool.WithReadOnly(), tool.WithConcurrent())
}

// runSearch implements search_tools: it lists the matching deferred tools
// and unlocks them, so the next request carries their schemas.
func (m *Middleware) runSearch(ctx context.Context, in SearchInput, c *tool.Call) (tool.Result, error) {
	info, ok := agent.Info(ctx)
	if !ok {
		return tool.Errorf("%s can only run inside an agent run", ToolSearch), nil
	}
	var candidates []tool.Tool
	for _, t := range info.Tools() {
		if m.isDeferred(t) && !gatedLocked(c.State, t) && !agent.ToolHidden(ctx, t.Name()) {
			candidates = append(candidates, t)
		}
	}
	found, err := m.search(candidates, in)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	if len(found) == 0 {
		return tool.Text(noMatchText), nil
	}
	entries := make([]string, len(found))
	names := make([]string, len(found))
	for i, t := range found {
		entries[i] = describe(t)
		names[i] = t.Name()
	}
	Unlock(c.State, names...)
	recordSearched(c.State, names)
	return tool.Text(strings.Join(entries, "\n\n")), nil
}

// describe renders one search_tools match.
func describe(t tool.Tool) string {
	schema, err := json.Marshal(t.Schema())
	if err != nil {
		schema = []byte(fmt.Sprintf("unavailable: %v", err))
	}
	return "### " + t.Name() + "\n" + t.Description() + "\nInput schema: " + string(schema)
}

// search returns the candidates in matches, best first, at most the
// maximum in.MaxResults asks for (see limit).
func (m *Middleware) search(candidates []tool.Tool, in SearchInput) ([]tool.Tool, error) {
	var found []tool.Tool
	switch {
	case strings.HasPrefix(in.Query, selectPrefix):
		found = selectNames(candidates, strings.TrimPrefix(in.Query, selectPrefix))
	case in.Regex:
		re, err := regexp.Compile("(?i)" + in.Query)
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression: %w", err)
		}
		found = matchRegex(candidates, re)
	default:
		found = rankKeywords(candidates, in.Query)
	}
	return found[:min(len(found), m.limit(in.MaxResults))], nil
}

// limit is how many matches a search returns when the model asks for
// requested: the configured default when requested is not positive, never
// above MaxResults.
func (m *Middleware) limit(requested int) int {
	if requested <= 0 {
		return m.maxResults
	}
	return min(requested, MaxResults)
}

// selectNames returns the candidates named in list, a comma-separated list
// of exact names, in its order; unknown names are skipped.
func selectNames(candidates []tool.Tool, list string) []tool.Tool {
	var found []tool.Tool
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		named := func(t tool.Tool) bool { return t.Name() == name }
		// Compared by name: tool values need not be comparable.
		if i := slices.IndexFunc(candidates, named); i >= 0 && !slices.ContainsFunc(found, named) {
			found = append(found, candidates[i])
		}
	}
	return found
}

// matchRegex returns the candidates whose name and description, joined by a
// newline, match re, sorted by name.
func matchRegex(candidates []tool.Tool, re *regexp.Regexp) []tool.Tool {
	var found []tool.Tool
	for _, t := range candidates {
		if re.MatchString(t.Name() + "\n" + t.Description()) {
			found = append(found, t)
		}
	}
	slices.SortFunc(found, byName)
	return found
}

// Keyword scores: a query term found in a tool's name counts more than one
// found in its description.
const (
	nameHitScore        = 2
	descriptionHitScore = 1
)

// rankKeywords scores the candidates against the lowercased words of query
// (see nameHitScore) and returns those that scored, best first, ties by
// name.
func rankKeywords(candidates []tool.Tool, query string) []tool.Tool {
	terms := strings.Fields(strings.ToLower(query))
	type scored struct {
		tool  tool.Tool
		score int
	}
	var hits []scored
	for _, t := range candidates {
		name, description := strings.ToLower(t.Name()), strings.ToLower(t.Description())
		score := 0
		for _, term := range terms {
			if strings.Contains(name, term) {
				score += nameHitScore
			}
			if strings.Contains(description, term) {
				score += descriptionHitScore
			}
		}
		if score > 0 {
			hits = append(hits, scored{t, score})
		}
	}
	slices.SortFunc(hits, func(a, b scored) int {
		return cmp.Or(cmp.Compare(b.score, a.score), byName(a.tool, b.tool))
	})
	found := make([]tool.Tool, len(hits))
	for i, h := range hits {
		found[i] = h.tool
	}
	return found
}

func byName(a, b tool.Tool) int { return cmp.Compare(a.Name(), b.Name()) }

// callTool returns call_tool: a tool.Dispatcher, so the agent loop runs the
// named tool in its place. Rules, hitl and hook matchers see that target;
// policy for call_tool itself belongs in permission rules naming it, which
// the loop evaluates first, on call_tool's own input. It has no effect of
// its own (the target is checked as if called directly), so it is
// read-only.
func callTool() tool.Tool {
	return tool.New(ToolCall,
		"Call a tool by its exact name, such as one search_tools found, with its input object.",
		func(context.Context, CallInput, *tool.Call) (tool.Result, error) {
			// The loop dispatches every valid call; reaching here means
			// call_tool was run outside it.
			return tool.Errorf("%s must be dispatched by the agent loop", ToolCall), nil
		},
		tool.WithReadOnly(),
		func(s *tool.Spec) { s.Dispatch = dispatch })
}

// dispatch is call_tool's tool.Dispatcher: it returns the named tool and its
// input, {} when the input is missing or null.
func dispatch(in json.RawMessage) (string, json.RawMessage, error) {
	var ci CallInput
	if err := json.Unmarshal(in, &ci); err != nil {
		return "", nil, fmt.Errorf("invalid %s input: %w", ToolCall, err)
	}
	if ci.Name == "" {
		return "", nil, errors.New("name is required")
	}
	if len(ci.Input) == 0 || string(ci.Input) == "null" {
		return ci.Name, json.RawMessage(`{}`), nil
	}
	return ci.Name, ci.Input, nil
}
