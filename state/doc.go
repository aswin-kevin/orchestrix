// Package state provides the typed, JSON-serializable, concurrency-safe state
// that agents, middleware and checkpointers share.
//
// A State holds the conversation messages plus named values. Values are
// accessed through typed Key handles; data loaded from JSON stays serialized
// until a Key first reads it, so a State can carry entries whose Go types the
// current process does not know without losing them.
//
// The JSON form of a State ({"version":1,"messages":…,"values":…}) is part
// of the compatibility promise: v1 releases read every layout an earlier v1
// wrote; a layout change bumps version.
package state
