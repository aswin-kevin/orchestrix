package backend

import (
	"context"
	"encoding/json"
	"time"
)

// FileInfo describes one directory entry.
type FileInfo struct {
	// Path is the entry's backend path.
	Path string
	// IsDir reports whether the entry is a directory.
	IsDir bool
	// Size is the size in bytes.
	Size int64
	// ModTime is the last modification time.
	ModTime time.Time
}

// ReadResult is the outcome of Backend.Read.
type ReadResult struct {
	// Lines are the lines read, without line terminators.
	Lines     []string
	StartLine int // 0-based index of Lines[0]
	// TotalLines is the number of lines in the whole file.
	TotalLines int
	// Binary is set for non-text files; Lines is then empty.
	Binary bool
	// MediaType is the media type of a binary file.
	MediaType string
	Data      []byte // binary files only
	// ModTime is the file's last modification time.
	ModTime time.Time
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
	Path string // directory or file to search; empty means the root
	Glob string // optional file-name filter
	// Mode selects what Grep reports; empty means GrepFiles.
	Mode GrepMode
	// IgnoreCase matches case-insensitively.
	IgnoreCase bool
	Literal    bool // treat the pattern as plain text instead of RE2
	// MaxResults caps the results; 0 or negative means the implementation
	// default (DefaultMaxResults).
	MaxResults int
}

// GrepMatch is one result of Backend.Grep. Line and Text are set in content
// mode, Count in count mode.
type GrepMatch struct {
	// Path is the file the result is for.
	Path string
	// Line is the 1-based line number, set in content mode.
	Line int
	// Text is the matching line, set in content mode.
	Text string
	// Count is the number of matching lines in the file, set in count mode.
	Count int
}

// Backend is the file-system contract used by file tools. Optional
// capabilities (Executor, Starter, Snapshotter, MultiEditor, Closer) are
// further interfaces a Backend may implement; look them up with AsExecutor,
// AsStarter, AsSnapshotter, AsMultiEditor and AsCloser rather than a type
// assertion.
//
// A decorator that wraps another Backend should implement
// Unwrap() Backend returning it, so the As helpers find the wrapped
// backend's capabilities through the decorator; the outermost implementer
// wins. A decorator that restricts a capability implements it itself (or
// does not Unwrap), so the unrestricted one underneath is never reached.
// Executor and Starter both run commands; a decorator that restricts commands
// must implement both (or not Unwrap), otherwise the inner Starter is reached
// through Unwrap.
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
	// Timeout stops the command after this long; 0 means the backend
	// default, and implementations treat a negative value as 0.
	Timeout time.Duration
	// Env holds variables that override the backend's environment; nil adds
	// none. Implementations must not modify it. A key must be non-empty and
	// contain no '=' or NUL; backends may reject one that does.
	Env map[string]string
	// Dir is the working directory as a backend path; empty means the root.
	Dir string
	// MaxOutputBytes caps the captured output; 0 means the backend default,
	// and implementations treat a negative value as 0.
	MaxOutputBytes int
}

// ExecResult is the outcome of Executor.Execute.
type ExecResult struct {
	// Output is the captured output of the command, cut at
	// ExecOptions.MaxOutputBytes. A cut Output may carry a short marker
	// saying how much was omitted, so it may exceed MaxOutputBytes by at
	// most 64 bytes; Truncated is then set.
	Output string
	// ExitCode is the command's exit status; -1 when it was killed by a
	// signal or timed out.
	ExitCode int
	// Truncated reports that Output was cut at MaxOutputBytes.
	Truncated bool
	// TimedOut reports that the command was stopped at ExecOptions.Timeout.
	TimedOut bool
}

// Executor runs shell commands.
type Executor interface {
	Execute(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)
}

// InternalDir is the hidden workspace directory where middleware keep the
// artifacts they write for the model to read back: offloaded transcripts,
// oversized tool results and messages. Those files may contain anything the
// conversation did, secrets included, so keep the directory out of version
// control (for a disk workspace, add ".orchestrix/" to .gitignore).
const InternalDir = "/.orchestrix"

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
	// Stop asks the command to end; Done closes afterwards. ctx bounds the
	// request itself (a remote Stop that calls out), not the wait for Done.
	// Stopping a finished process is a no-op.
	Stop(ctx context.Context) error
}

// Starter starts a command without waiting for it. The process is not tied to
// ctx (ctx bounds only the start); it ends on completion, opts.Timeout, or
// Process.Stop.
type Starter interface {
	Start(ctx context.Context, cmd string, opts ExecOptions) (Process, error)
}

// ExecStarter is an executor that can also start commands without waiting
// for them.
type ExecStarter interface {
	Executor
	Starter
}

// Snapshotter is implemented by backends whose content can be captured into,
// and restored from, a JSON value (used for checkpoints). ctx bounds the
// capture or the restore.
type Snapshotter interface {
	// Snapshot captures the backend's content.
	Snapshot(ctx context.Context) (json.RawMessage, error)
	// Restore replaces the backend's content with a captured one.
	Restore(ctx context.Context, data json.RawMessage) error
}

// Closer is implemented by a backend that holds resources (a directory,
// containers, connections) to release when it is no longer used. ctx bounds
// how long Close may wait for them to end.
type Closer interface {
	Close(ctx context.Context) error
}

// EditOp is one replacement within a MultiEditor.MultiEdit call.
type EditOp struct {
	// Old is the text to replace and New is its replacement; Old must not be
	// empty or equal to New.
	Old, New string
	// ReplaceAll replaces every occurrence of Old; otherwise Old must be
	// unique in the file.
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
