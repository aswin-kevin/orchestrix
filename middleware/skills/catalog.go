package skills

import (
	"context"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/state"
)

// neutralizeTags escapes any tag in skill text that could open or end one of
// the segment's own elements.
var neutralizeTags = promptsafe.NewTagNeutralizer("available_skills", "skill")

// BeforeAgent implements agent.BeforeAgent: a pinned skill (Config.Pinned
// and the run's Scope.Pinned) is in effect from the first turn, so the
// tools it names in allowed-tools and the tools of its MCP servers are
// unlocked as if it had been loaded.
//
// A pin that is gone since the thread's last run (the host dropped it from
// Config.Pinned or Scope.Pinned) is taken back: its unlock patterns are
// locked again, except those a current pin, a loaded skill or search_tools
// also unlocked. A State saved before this bookkeeping existed has no record
// of earlier pins, so nothing is locked again for it.
func (m *Middleware) BeforeAgent(_ context.Context, s *state.State) error {
	scope, _ := ScopeKey.Get(s)
	var want []string
	for _, name := range slices.Concat(m.pinned, scope.Pinned) {
		if sk, ok := m.byName[name]; ok {
			want = append(want, unlockPatterns(sk)...)
		}
	}
	keep := slices.Concat(want, deferred.Searched(s))
	loaded, _ := LoadedKey.Get(s)
	for _, name := range loaded {
		if sk, ok := m.byName[name]; ok {
			keep = append(keep, unlockPatterns(sk)...)
		}
	}
	previous, _ := pinnedUnlocksKey.Get(s)
	deferred.Lock(s, slices.DeleteFunc(slices.Clone(previous), func(p string) bool {
		return slices.Contains(keep, p)
	})...)
	deferred.Unlock(s, want...)
	pinnedUnlocksKey.Set(s, slices.Compact(slices.Sorted(slices.Values(want))))
	return nil
}

// WrapModel implements agent.ModelWrapper: it appends the catalog segment,
// after the host's own segments, listing the skills the run is offered and the
// body of each pinned skill. Then it re-injects the instructions of loaded
// skills the request messages no longer show (see StandingHeader).
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	scope, _ := ScopeKey.Get(req.State)
	if seg := m.segment(scope); seg != "" {
		req.Request.System = append(slices.Clone(req.Request.System), seg)
	}
	req.Request.Messages = m.standing(req.State, scope, req.Request.Messages)
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
		parts = append(parts, frame(s))
	}
	return strings.Join(parts, "\n\n")
}

// catalog renders the prompt and the <available_skills> list.
func (m *Middleware) catalog(scope Scope) string {
	var list strings.Builder
	for _, s := range m.skills {
		if _, ok := m.offered(scope, s.Name); !ok {
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
