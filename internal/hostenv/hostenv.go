// Package hostenv builds the environment of child processes from the host's,
// shared by the shell backend and the MCP stdio transport so both pass the
// same variables.
package hostenv

import (
	"maps"
	"os"
	"slices"
)

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
