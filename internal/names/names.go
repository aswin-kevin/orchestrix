// Package names holds the naming rules shared by the tool and permission
// packages, so a tool can never be registered under a name that a permission
// rule is unable to address.
package names

import "fmt"

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
