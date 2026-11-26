// Package composite routes each path to one of several backends by its longest
// prefix and merges directory listings, globs and greps across them. Mount
// memory.DefaultDir onto a persistent backend such as backend/kv to keep
// memories across threads while the rest of the workspace stays per thread.
//
// Backend has no move or rename, so no edit ever crosses routes. Commands see
// only the default backend: mounted files are invisible to them, and def files
// a mount shadows remain visible.
//
// The composite owns its backends: its Close (see New) closes the default
// backend and every route's backend.
package composite
