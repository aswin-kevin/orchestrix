package filesystem

import (
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/tool"
)

var errNoBackend = errors.New("no backend configured")

// errorResult renders err for the model as a concise error result. Backend
// failures are expected conditions the model can recover from, so they are
// results rather than Go errors.
func errorResult(err error, path string) tool.Result {
	switch {
	case errors.Is(err, backend.ErrNotFound):
		return tool.Errorf("File not found: %s", path)
	case errors.Is(err, backend.ErrIsDir):
		return tool.Errorf("%s is a directory", path)
	case errors.Is(err, backend.ErrNotDir):
		return tool.Errorf("%s is not a directory", path)
	case errors.Is(err, backend.ErrOutsideRoot):
		return tool.Errorf("%s is outside the workspace", path)
	case errors.Is(err, backend.ErrNoMatch):
		return tool.Errorf("old_string not found in %s", path)
	case errors.Is(err, backend.ErrAmbiguous):
		return tool.Errorf("old_string matches multiple times in %s; add context or set replace_all", path)
	}
	return tool.Errorf("%v", err)
}

// stepError prefixes an edit failure with the position of the failing edit.
func stepError(i, total int, res tool.Result) tool.Result {
	return tool.Errorf("edit %d of %d failed: %s; no changes were made", i+1, total, res.Flatten())
}

// staleReadMessage is what the model sees when it edits a file it has not
// read, or one that changed after it read it.
const staleReadMessage = "File has not been read (or changed since it was read). Read it first."

// textf is tool.Text with a format string.
func textf(format string, a ...any) tool.Result {
	return tool.Text(fmt.Sprintf(format, a...))
}
