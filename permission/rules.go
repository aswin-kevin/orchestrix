package permission

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/internal/glob"
	"github.com/aswin-kevin/orchestrix/internal/names"
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
	// Tool is the tool name a tool rule matches; empty for a path rule.
	Tool string
	// Pattern narrows a tool rule against Request.Subject, or is the path
	// pattern of a path rule.
	Pattern string
	// Op is the access a path rule matches; empty for a tool rule.
	Op Op
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
		if err := names.Validate(s); err != nil {
			return Rule{}, fmt.Errorf("permission rule %q: %w", s, err)
		}
		return Rule{Tool: s}, nil
	}
	if !strings.HasSuffix(s, ")") {
		return Rule{}, fmt.Errorf("permission rule %q: unbalanced parentheses", s)
	}
	name, pattern := s[:open], s[open+1:len(s)-1]
	if err := names.Validate(name); err != nil {
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

// matchesCommand reports whether the rule is a tool rule for tool whose
// pattern matches one command of a shell subject.
func (r Rule) matchesCommand(tool, command string) bool {
	return !r.isPathRule() && r.Tool == tool && r.Pattern != "" && matchSubject(r.Pattern, command)
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

// matchesPathsFolded is matchesPaths with the pattern and every path compared
// as a case-insensitive file system sees them (see foldPath), so that a deny
// or an ask rule for "/secrets/**" also stops "/SECRETS/key" and
// "/secrets./key", which macOS and Windows open as the same file. Only deny
// and ask rules use it: on a case-sensitive file system it makes them match
// more often, which fails closed. Allow rules always match exactly, so
// folding never widens what is allowed.
func (r Rule) matchesPathsFolded(accesses []PathAccess) bool {
	if !r.isPathRule() {
		return false
	}
	pattern := foldPath(r.Pattern)
	for _, a := range accesses {
		if a.Op == r.Op && glob.Match(pattern, foldPath("/"+a.Path)) {
			return true
		}
	}
	return false
}

// foldPath returns p with each component as a case-insensitive file system
// that ignores trailing dots and spaces, as Windows does, sees it (see
// fileName). p is cleaned first, so "." and ".." are resolved before folding
// and cannot be trimmed into something else; a component left empty by the
// trimming (such as "...") then drops out when the result is cleaned again.
// Glob metacharacters are unaffected, so p may be a pattern.
func foldPath(p string) string {
	components := strings.Split(path.Clean(p), "/")
	for i, c := range components {
		components[i] = fileName(c)
	}
	return path.Clean(strings.Join(components, "/"))
}

// ReadVisible reports whether a search tool (ls, glob, grep) may show the
// path p in its results. Rules only see the root a search starts from, so the
// tool must filter what it finds below that root with this check.
//
// A path is hidden when a deny or an ask path rule for reading matches it,
// ignoring case as deny and ask rules do (see matchesPathsFolded).
// Ask-gated paths are hidden as well (fail closed): a search reports many
// paths at once and cannot pause to ask about each one, so the safe answer is
// to leave them out. Reading such a file directly with a read tool still asks.
func (r Rules) ReadVisible(p string) bool {
	access := []PathAccess{{Path: p, Op: OpRead}}
	for _, rule := range slices.Concat(r.Deny, r.Ask) {
		if rule.matchesPathsFolded(access) {
			return false
		}
	}
	return true
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
