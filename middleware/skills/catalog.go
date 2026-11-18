package skills

import (
	"context"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/llm"
)

// neutralizeTags escapes any tag in skill text that could open or end one of
// the segment's own elements.
var neutralizeTags = promptsafe.NewTagNeutralizer("available_skills", "skill")

// WrapModel implements agent.ModelWrapper: it appends the catalog segment,
// after the host's own segments, listing the skills the run is offered and the
// body of each pinned skill.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	scope, _ := ScopeKey.Get(req.State)
	if seg := m.segment(scope); seg != "" {
		req.Request.System = append(slices.Clone(req.Request.System), seg)
	}
	return next(ctx, req)
}

// segment renders the catalog and the pinned skills for scope; it is empty
// when there is nothing to show.
func (m *Middleware) segment(scope Scope) string {
	var parts []string
	if catalog := m.catalog(scope); catalog != "" {
		parts = append(parts, catalog)
	}
	seen := map[string]bool{}
	for _, name := range slices.Concat(m.pinned, scope.Pinned) {
		s, ok := m.byName[name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		body, _ := promptsafe.CapBytes(neutralizeTags(s.Body), DefaultMaxBodyBytes)
		parts = append(parts, "<skill name=\""+s.Name+"\">\n"+body+"\n</skill>")
	}
	return strings.Join(parts, "\n\n")
}

// catalog renders the prompt and the <available_skills> list.
func (m *Middleware) catalog(scope Scope) string {
	var list strings.Builder
	for _, s := range m.skills {
		if scope.Restrict && !slices.Contains(scope.Allowed, s.Name) {
			continue
		}
		list.WriteString("- " + s.Name + ": " + oneLine(s.Description) + "\n")
	}
	if list.Len() == 0 {
		return ""
	}
	return m.prompt + "<available_skills>\n" + list.String() + "</available_skills>"
}

// oneLine collapses all whitespace, so a description cannot start a line of
// its own, and neutralizes frame tags.
func oneLine(s string) string {
	return neutralizeTags(strings.Join(strings.Fields(s), " "))
}
