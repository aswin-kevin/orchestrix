// Package backend defines the storage and execution contracts that tools use
// to touch files and run commands. Paths are virtual and absolute ("/src/a.go");
// each implementation decides how they map to real storage.
package backend

import (
	"context"
	"encoding/json"
	"time"
)

// FileInfo describes one directory entry.
type FileInfo struct {
	Path    string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// ReadResult is the outcome of Backend.Read.
type ReadResult struct {
	Lines      []string
	StartLine  int // 0-based index of Lines[0]
	TotalLines int
	// Binary is set for non-text files; Lines is then empty.
	Binary    bool
	MediaType string
	Data      []byte // binary files only
	ModTime   time.Time
}

// EditResult is the outcome of Backend.Edit.
type EditResult struct {
	Occurrences int // number of replacements made
}

// GrepMode selects what Backend.Grep reports.
type GrepMode string

// Grep output modes.
const (
	GrepFiles   GrepMode = "files_with_matches"
	GrepContent GrepMode = "content"
	GrepCount   GrepMode = "count"
)

// GrepOptions configures Backend.Grep.
type GrepOptions struct {
	Path       string // directory or file to search; empty means the root
	Glob       string // optional file-name filter
	Mode       GrepMode
	IgnoreCase bool
	Literal    bool // treat the pattern as plain text instead of RE2
	MaxResults int  // 0 means the implementation default (1000)
}

// GrepMatch is one result of Backend.Grep. Line and Text are set in content
// mode, Count in count mode.
type GrepMatch struct {
	Path  string
	Line  int
	Text  string
	Count int
}

// Backend is the file-system contract used by file tools.
type Backend interface {
	// Ls lists the direct children of a directory.
	Ls(ctx context.Context, path string) ([]FileInfo, error)
	// Read returns up to limit lines starting at the 0-based line offset. A
	// file over MaxReadFileBytes fails with ErrTooLarge, as it does for Edit
	// and MultiEdit.
	Read(ctx context.Context, path string, offset, limit int) (ReadResult, error)
	// Write replaces the file's content, creating parent directories.
	Write(ctx context.Context, path string, content []byte) error
	// Edit replaces old with new. It fails with ErrNoMatch when old is absent
	// and with ErrAmbiguous when old is not unique and replaceAll is false.
	// An empty old, or old equal to new, is an error (ErrInvalidEdit).
	Edit(ctx context.Context, path, old, new string, replaceAll bool) (EditResult, error)
	// Delete removes a file, or a directory recursively.
	Delete(ctx context.Context, path string) error
	// Glob finds files under path matching pattern, newest first.
	Glob(ctx context.Context, pattern, path string) ([]FileInfo, error)
	// Grep searches file contents.
	Grep(ctx context.Context, pattern string, opts GrepOptions) ([]GrepMatch, error)
}

// ExecOptions configures Executor.Execute and Starter.Start.
type ExecOptions struct {
	// Timeout stops the command after this long; 0 means the backend default.
	Timeout time.Duration
	// Env holds extra "KEY=value" entries that override the backend's
	// environment; nil adds none.
	Env []string
	// Dir is the working directory as a backend path; empty means the root.
	Dir string
	// MaxOutputBytes caps the captured output; 0 means the backend default.
	MaxOutputBytes int
}

// ExecResult is the outcome of Executor.Execute.
type ExecResult struct {
	Output   string
	ExitCode int
	// Truncated reports that Output was cut at MaxOutputBytes.
	Truncated bool
	TimedOut  bool
}

// Executor runs shell commands.
type Executor interface {
	Execute(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)
}

// DefaultKillGrace is how long a stopped command's process group gets to exit
// after SIGTERM before it is sent SIGKILL. Callers waiting for a stopped
// Process can bound the wait by it.
const DefaultKillGrace = 2 * time.Second

// Process is a command started by a Starter.
type Process interface {
	// Done is closed once the command has finished and its whole process
	// group is gone.
	Done() <-chan struct{}
	// Result returns the final result; ok is false until Done is closed.
	Result() (res ExecResult, ok bool)
	// Output returns the output captured so far, capped like
	// ExecResult.Output. It is safe to call while the command runs.
	Output() string
	// Stop kills the process group; Done closes afterwards. Stopping a
	// finished process is a no-op.
	Stop() error
}

// Starter starts a command without waiting for it. The process is not tied to
// ctx (ctx bounds only the start); it ends on completion, opts.Timeout, or
// Process.Stop.
type Starter interface {
	Start(ctx context.Context, cmd string, opts ExecOptions) (Process, error)
}

// Snapshotter is implemented by backends whose content can be captured into,
// and restored from, a JSON value (used for checkpoints).
type Snapshotter interface {
	Snapshot() (json.RawMessage, error)
	Restore(json.RawMessage) error
}

// EditOp is one replacement within a MultiEditor.MultiEdit call.
type EditOp struct {
	Old, New   string
	ReplaceAll bool
}

// MultiEditor is an optional capability of a Backend: applying several edits
// to one file as a single all-or-nothing operation. Edits apply in order, each
// seeing the result of the previous ones, to the file's exact bytes. If any
// edit fails the file is left untouched and the error is an *EditStepError.
// EditResult.Occurrences is the total number of replacements.
type MultiEditor interface {
	MultiEdit(ctx context.Context, path string, edits []EditOp) (EditResult, error)
}
