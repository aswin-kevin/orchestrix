package backend

import (
	"fmt"
	"path"
	"strings"

	"github.com/aswin-kevin/orchestrix/internal/glob"
)

// Clean converts p to a cleaned virtual absolute path such as "/a/b". Relative
// input is rooted at "/", and ".." can never climb above it. Empty input and
// NUL bytes are rejected with an error wrapping ErrInvalidPath.
func Clean(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: contains NUL byte", ErrInvalidPath)
	}
	return path.Clean("/" + p), nil
}

// MatchGlob reports whether virtual path p matches the glob pattern; it is the
// matcher every backend uses so results agree across implementations.
func MatchGlob(pattern, p string) bool {
	return glob.Match(pattern, p)
}
