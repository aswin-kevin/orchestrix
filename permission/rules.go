package permission

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aswin-kevin/orchestrix/internal/glob"
)

const (
	// prefixSuffix marks a prefix pattern such as "git:*".
	prefixSuffix = ":*"
	// prefixSeparator is what must follow the prefix in a command.
	prefixSeparator = " "
	wildcard        = '*'
)

// Rule is a single permission rule. A tool rule has Tool set and matches calls
// to that tool, optionally narrowed by Pattern against Request.Subject. A path
// rule has Tool empty and Op set, and matches calls that access a path
// matching Pattern with that Op.
type Rule struct {
	Tool    string
	Pattern string
	Op      Op
}

// ParseRule parses the textual rule forms "Tool", "Tool(pattern)",
// "read(glob)" and "write(glob)". Surrounding whitespace is ignored.
//
// The lowercase keywords read and write only denote path rules when followed
// by parentheses; a bare "read" is an ordinary tool rule for a tool named
// "read".
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, errors.New("permission rule: empty rule")
	}
	open := strings.IndexByte(s, '(')
	if open < 0 {
		if strings.ContainsRune(s, ')') {
			return Rule{}, fmt.Errorf("permission rule %q: unbalanced parentheses", s)
		}
		if err := validateToolName(s); err != nil {
			return Rule{}, fmt.Errorf("permission rule %q: %w", s, err)
		}
		return Rule{Tool: s}, nil
	}
	if !strings.HasSuffix(s, ")") {
		return Rule{}, fmt.Errorf("permission rule %q: unbalanced parentheses", s)
	}
	name, pattern := s[:open], s[open+1:len(s)-1]
	if err := validateToolName(name); err != nil {
		return Rule{}, fmt.Errorf("permission rule %q: %w", s, err)
	}
	if pattern == "" {
		return Rule{}, fmt.Errorf("permission rule %q: empty pattern", s)
	}
	// Commands may contain parentheses, but only balanced ones; a stray ")"
	// usually means a typo that would silently change what the rule matches.
	if !balancedParens(pattern) {
		return Rule{}, fmt.Errorf("permission rule %q: unbalanced parentheses", s)
	}
	switch Op(name) {
	case OpRead, OpWrite:
		return Rule{Pattern: pattern, Op: Op(name)}, nil
	}
	return Rule{Tool: name, Pattern: pattern}, nil
}

// validateToolName rejects names with characters outside [A-Za-z0-9_.:-]
// (which covers MCP-style names such as "mcp__srv__tool"). Without this, a typo
// like "write (/x)" would become a tool rule for a nonexistent tool and the
// rule would silently never match.
func validateToolName(name string) error {
	if name == "" {
		return errors.New("missing tool name")
	}
	for _, c := range name {
		if !isToolNameChar(c) {
			return fmt.Errorf("invalid character %q in tool name", c)
		}
	}
	return nil
}

func isToolNameChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '.' || c == ':' || c == '-'
}

// balancedParens reports whether every ")" in s closes an earlier "(".
func balancedParens(s string) bool {
	depth := 0
	for _, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// String renders the rule in the form ParseRule accepts.
func (r Rule) String() string {
	name := r.Tool
	if r.isPathRule() {
		name = string(r.Op)
	}
	if r.Pattern == "" {
		return name
	}
	return name + "(" + r.Pattern + ")"
}

func (r Rule) isPathRule() bool { return r.Op != "" }

// Matches reports whether the rule applies to req.
func (r Rule) Matches(req Request) bool {
	if r.isPathRule() {
		return r.matchesPaths(req.Paths)
	}
	if r.Tool != req.Tool {
		return false
	}
	return r.Pattern == "" || matchSubject(r.Pattern, req.Subject)
}

// matchesPaths is true when any single access has the rule's Op and a path
// matching the rule's glob. Paths are virtual absolute paths; a relative one is
// rooted at "/" before matching so that "secrets/x" cannot slip past a rule
// written for "/secrets/**" (fail closed).
func (r Rule) matchesPaths(accesses []PathAccess) bool {
	for _, a := range accesses {
		if a.Op == r.Op && glob.Match(r.Pattern, "/"+a.Path) {
			return true
		}
	}
	return false
}

// matchSubject matches a tool rule pattern against a call's Subject. The
// "prefix:*" form matches the prefix alone or the prefix followed by a space,
// so "git:*" accepts "git" and "git status" but not "gitx". Any other pattern
// is a plain wildcard match.
func matchSubject(pattern, subject string) bool {
	if prefix, ok := strings.CutSuffix(pattern, prefixSuffix); ok {
		return subject == prefix || strings.HasPrefix(subject, prefix+prefixSeparator)
	}
	return wildcardMatch(pattern, subject)
}

// wildcardMatch matches text against a pattern where '*' stands for any run of
// characters, including '/'. Subjects such as commands and URLs are not paths,
// so segment-aware path globbing would be wrong for them.
func wildcardMatch(pattern, text string) bool {
	parts := strings.Split(pattern, string(wildcard))
	if len(parts) == 1 {
		return pattern == text
	}
	first, last := parts[0], parts[len(parts)-1]
	if !strings.HasPrefix(text, first) {
		return false
	}
	text = text[len(first):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(text, mid)
		if i < 0 {
			return false
		}
		text = text[i+len(mid):]
	}
	return strings.HasSuffix(text, last)
}
