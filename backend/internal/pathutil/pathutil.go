package pathutil

import (
	"fmt"

	"github.com/aswin-kevin/orchestrix/backend"
)

// CleanBase cleans a directory argument, treating "" as the root "/".
func CleanBase(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	return backend.Clean(p)
}

// Fail returns "<prefix>: <op> <p>: <sentinel>", wrapping sentinel.
func Fail(prefix, op, p string, sentinel error) error {
	return fmt.Errorf("%s: %s %s: %w", prefix, op, p, sentinel)
}
