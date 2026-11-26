package hostenv

import (
	"maps"
	"os"
	"slices"
	"strings"
)

// defaultNames backs DefaultNames; it is never handed out.
var defaultNames = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TMPDIR", "TZ"}

// DefaultNames returns the names of the host variables a child process
// receives by default: enough for tools to find binaries, a home and a
// locale, and nothing secret. Each call returns a fresh copy.
func DefaultNames() []string { return slices.Clone(defaultNames) }

// Inherited returns the host variables a child receives: the whole host
// environment when all is set, otherwise only the named variables that are
// set, in the order named.
func Inherited(names []string, all bool) []string {
	if all {
		return os.Environ()
	}
	var env []string
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// Pairs returns vars as KEY=VALUE entries sorted by key, so the resulting
// environment is deterministic; nil for an empty map.
func Pairs(vars map[string]string) []string {
	var env []string
	for _, key := range slices.Sorted(maps.Keys(vars)) {
		env = append(env, key+"="+vars[key])
	}
	return env
}

// InvalidKey returns the first key of vars, in sorted order, that cannot
// name an environment variable (empty, or holding '=' or NUL), and whether
// there is one.
func InvalidKey(vars map[string]string) (string, bool) {
	for _, key := range slices.Sorted(maps.Keys(vars)) {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return key, true
		}
	}
	return "", false
}
