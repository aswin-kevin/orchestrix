package filesystem

import (
	"encoding/json"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/permission"
)

// rootPath is the default for tools that search a directory.
const rootPath = "/"

// accessFor builds the tool.WithPaths function reporting the one path a tool
// touches. The path is reported cleaned and absolute, so permission rules see
// the same location the backend will. A path that cannot be cleaned is
// reported as given: the call will fail anyway, and dropping it would hide an
// access from the rules.
//
// Backslashes count as separators here on every OS: a Windows backend
// resolves them, so x\..\secrets\key must reach the rules as
// "/secrets/key". On Unix, where a backslash may be part of a name, this only
// makes rules match more often (fail closed); the backend still opens the
// path as given.
func accessFor[In any](op permission.Op, pathOf func(In) string) func(json.RawMessage) []permission.PathAccess {
	return func(raw json.RawMessage) []permission.PathAccess {
		var in In
		_ = json.Unmarshal(raw, &in) // malformed input falls back to the root, the strictest answer
		return []permission.PathAccess{{Path: reportedPath(pathOf(in)), Op: op}}
	}
}

func reportedPath(p string) string {
	if p == "" {
		return rootPath
	}
	if clean, err := backend.Clean(strings.ReplaceAll(p, `\`, "/")); err == nil {
		return clean
	}
	return p
}

// orRoot returns p, or the root when p is empty.
func orRoot(p string) string {
	if p == "" {
		return rootPath
	}
	return p
}
