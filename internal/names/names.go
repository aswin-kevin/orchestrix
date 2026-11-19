// Package names holds the naming rules shared by the tool and permission
// packages, so a tool can never be registered under a name that a permission
// rule is unable to address.
package names

import (
	"fmt"
	"strings"
)

// mcpToolPrefix starts the name of every tool an MCP server provides.
const mcpToolPrefix = "mcp__"

// Validate reports why name is not a legal tool name. Legal names are
// non-empty and use only [A-Za-z0-9_.:-], which covers MCP-style names such
// as "mcp__srv__tool".
func Validate(name string) error {
	if name == "" {
		return fmt.Errorf("missing tool name")
	}
	for _, c := range name {
		if !isNameChar(c) {
			return fmt.Errorf("invalid character %q in tool name", c)
		}
	}
	return nil
}

func isNameChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '.' || c == ':' || c == '-'
}

// Sanitize returns s with every rune outside [A-Za-z0-9_-] replaced by '_',
// so any server or tool name can become part of a legal tool name.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// MCPPrefix is the prefix of the tool names of the MCP server named server:
// "mcp__" + Sanitize(server) + "__".
func MCPPrefix(server string) string {
	return mcpToolPrefix + Sanitize(server) + "__"
}
