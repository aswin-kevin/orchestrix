// Package sandbox provides a backend.Backend for a remote container or VM
// whose only primitive is running a shell command. An implementor supplies a
// backend.Executor; every file operation (read, write, edit, ls, glob, grep,
// delete) is built from POSIX sh scripts sent through it.
//
// Virtual paths map below Config.Root inside the sandbox: "/src/a.go" is
// <Root>/src/a.go. Paths are confined lexically (backend.Clean never climbs
// above "/"); symlinks inside the sandbox are followed by the commands, so
// the sandbox itself, not this package, is the security boundary.
//
// Quoting: every path reaches the shell as one POSIX single-quoted word built
// by a single helper, file content travels as base64 in chunks of ChunkBytes,
// and no caller data is ever placed in a printf format string. Hostile names
// (quotes, $(...), backticks, ";", newlines, leading "-", globs, non-UTF-8
// bytes) are therefore inert.
//
// Symlinks: commands follow them. Ls lists a symlink to a directory as a
// directory and a dangling symlink as a file of size 0 with a zero (Unix
// epoch) mtime, which Delete removes and Read reports as not found. Glob and
// Grep list regular files only, so they skip symlinks.
//
// Limits: modification times are read with whole-second resolution; a
// rewritten file gets default permissions rather than keeping its old mode;
// listings run wc and date per entry, so Glob and Grep over very large trees
// are slow. Grep prunes .git and node_modules directories, like backend/disk.
// Writes fill a temporary ".orx-tmp-<16 hex digits>" file next to the
// target, and listings hide names of exactly that form. Nothing sweeps them:
// a write cut off between chunks (the executor died, the sandbox restarted)
// leaves its temporary file behind, hidden but using space, until removed by
// hand. A file the sandbox user cannot read is listed with size 0 (and its
// real mtime); Grep skips it. A file that grows by more than a few KiB
// between the size check and the transfer in Read or Grep fails safely: the
// output exceeds the cap computed from the earlier size and the operation
// returns a truncation error, never partial content.
// A listing whose tools fail (an unsupported find flag, date -r erroring on
// an existing entry) is an error, never a silently empty result.
//
// Non-UTF-8 names: sizes and contents are read through shell redirection,
// which takes any bytes, but modification times need date -r FILE, which
// takes the name as an argument, and there is no portable way around that.
// Some userlands (the Rust coreutils that recent Ubuntu ships by default)
// refuse arguments that are not valid UTF-8. There, reading or listing such
// a name fails with "sandbox: this userland cannot handle non-UTF-8 file
// names: <path>" rather than with a raw tool error; GNU coreutils, busybox
// and the BSD tools handle any bytes.
//
// Background commands: the Backend New returns implements backend.Executor
// only. When the executor also implements backend.Starter, NewStarter
// returns a StarterBackend that starts commands the same way, which the jobs
// middleware (orchestrix.WithJobs) needs. orxdocker, a separate module,
// provides a Docker executor that does.
//
// Local implementation: wrapping a backend/shell Backend gives a sandbox on
// the host, useful for development and tests:
//
//	sh, err := shell.New(dir, shell.Config{})
//	...
//	b, err := sandbox.New(sh, sandbox.Config{Root: dir})
package sandbox
