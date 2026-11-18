package backend

import "errors"

// Sentinel errors returned (wrapped) by Backend implementations. Callers match
// them with errors.Is.
var (
	// ErrNotFound means the path does not exist.
	ErrNotFound = errors.New("backend: not found")
	// ErrIsDir means a file operation was applied to a directory.
	ErrIsDir = errors.New("backend: is a directory")
	// ErrNotDir means a directory operation was applied to a file.
	ErrNotDir = errors.New("backend: not a directory")
	// ErrOutsideRoot means the path resolves outside the backend's root.
	ErrOutsideRoot = errors.New("backend: path outside root")
	// ErrNoMatch means an edit's old text does not occur in the file.
	ErrNoMatch = errors.New("backend: no match")
	// ErrAmbiguous means an edit's old text occurs more than once and
	// replaceAll was not requested.
	ErrAmbiguous = errors.New("backend: ambiguous match")
	// ErrInvalidPath means a path is empty or contains a NUL byte.
	ErrInvalidPath = errors.New("backend: invalid path")
	// ErrInvalidEdit means an edit's old text is empty or equals its new text.
	ErrInvalidEdit = errors.New("backend: invalid edit")
)
