package backend

import (
	"errors"
	"fmt"
)

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
	// ErrTooLarge means a file is bigger than MaxReadFileBytes. The error
	// is a *TooLargeError, which carries the file's size.
	ErrTooLarge = errors.New("backend: file too large")
)

// MaxReadFileBytes is the largest file Read, Edit and MultiEdit load into
// memory. Larger files fail with ErrTooLarge, so one huge file cannot exhaust
// the host's memory; Grep skips them separately (see MaxGrepFileBytes).
const MaxReadFileBytes = 32 << 20

// TooLargeError reports a file over MaxReadFileBytes. It matches ErrTooLarge
// with errors.Is.
type TooLargeError struct {
	// Size is the file's size in bytes.
	Size int64
}

// Error implements error.
func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%v: %d bytes (limit %d)", ErrTooLarge, e.Size, MaxReadFileBytes)
}

// Unwrap returns ErrTooLarge.
func (e *TooLargeError) Unwrap() error { return ErrTooLarge }

// CheckReadSize returns a *TooLargeError when a file of size bytes is over
// MaxReadFileBytes, and nil otherwise. Backends call it before loading a file.
func CheckReadSize(size int64) error {
	if size > MaxReadFileBytes {
		return &TooLargeError{Size: size}
	}
	return nil
}
