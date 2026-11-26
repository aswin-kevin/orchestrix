// Package msgutil holds predicates and extractors over llm.Message shared
// by the middleware.
package msgutil

import (
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
)

// IsToolResult reports whether b is a tool result block.
func IsToolResult(b llm.Block) bool { return b.Type == llm.BlockToolResult }

// CarriesToolResult reports a user message holding at least one tool result.
func CarriesToolResult(m llm.Message) bool {
	return m.Role == llm.RoleUser && slices.ContainsFunc(m.Content, IsToolResult)
}

// IsUserTurn reports a user message that starts a turn: neither a set of
// tool results nor a message a middleware injected (agent.InjectedMetaKey).
func IsUserTurn(m llm.Message) bool {
	return m.Role == llm.RoleUser && !CarriesToolResult(m) && m.Meta[agent.InjectedMetaKey] == ""
}

// UserText joins the non-empty text of the user messages in msgs with
// newlines.
func UserText(msgs []llm.Message) string {
	var parts []string
	for _, m := range msgs {
		if t := m.Text(); m.Role == llm.RoleUser && t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}
